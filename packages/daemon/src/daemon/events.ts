import {
  controllerToken,
  EnvelopeSchema,
  type IssueKey,
  parseRoleToken,
  roleTopic,
} from "@legion/contracts";
import { type CheckRunRef, sortedCheckRunRefs } from "../state/types";
import type { DaemonConfig } from "./config";
import type { HeldEvent, LegionState, TreeState } from "./legion-state";
import {
  classifySettlement,
  type Effect,
  type EnvelopeJson,
  effectiveOutcome,
  type LegionEventPayload,
  reduceGithubEvent,
  refreshCiIdentity,
  settleCiVerdict,
  writeCiFence,
} from "./reducers";

const CHECKS_TOPIC = /^notifications\.github\.([^.]+)\.([^.]+)\.pr\.(\d+)\.checks$/;
const EXCEPTION_TOPIC = "notifications.envoy.exceptions.notifications.role.";
const MAX_RETRY_DELAY_MS = 30_000;
const INITIAL_RETRY_DELAY_MS = 1_000;

type JsonRecord = Record<string, unknown>;

export interface EventPumpDeps {
  nats: {
    subscribe(subject: string, cb: (subject: string, data: string) => void): () => void;
    publish(subject: string, data: string): void;
  };
  envoyPublish(topic: string, payloadJson: string): Promise<void>;
  state: LegionState;
  saveState(): Promise<void>;
  onException(ex: ExceptionInfo): Promise<void>;
  onLinger(tree: IssueKey): Promise<void>;
  onProbe(tree: IssueKey): Promise<void>;
  onApprovalStatus(effect: Extract<Effect, { kind: "approval-status" }>): Promise<void>;
  config: DaemonConfig;
}

export interface ExceptionInfo {
  roleToken: string;
  reason: "no_holder" | "delivery_failed";
  original: { topic: string; payload: string; eventId: string };
  controller?: true;
}

interface HeldTarget {
  heldEvents: HeldEvent[];
  isActive(): boolean;
}

interface ChecksInput {
  repo: `${string}/${string}`;
  number: number;
  sha: string;
  failed: string[];
  cancelledCount: number;
  settledAt: number;
  checkRuns: CheckRunRef[];
  generation: number;
  snapshot: string;
}

function asRecord(value: unknown): JsonRecord | undefined {
  return typeof value === "object" && value !== null && !Array.isArray(value)
    ? (value as JsonRecord)
    : undefined;
}

function stringValue(value: unknown): string | undefined {
  return typeof value === "string" ? value : undefined;
}

function recordPayload(envelope: EnvelopeJson): JsonRecord | undefined {
  if (typeof envelope.payload !== "string") return asRecord(envelope.payload);
  try {
    return asRecord(JSON.parse(envelope.payload));
  } catch {
    return undefined;
  }
}

function statusGroup(
  payload: JsonRecord,
  key: string
): { count: number; checks: string[] } | undefined {
  const group = asRecord(payload[key]);
  const count = group?.count;
  const checks = group?.checks;
  if (
    typeof count !== "number" ||
    !Number.isSafeInteger(count) ||
    count < 0 ||
    !Array.isArray(checks) ||
    !checks.every((check): check is string => typeof check === "string") ||
    checks.length !== count
  ) {
    return undefined;
  }
  return { count, checks };
}

function checksInput(subject: string, envelope: EnvelopeJson): ChecksInput | undefined {
  const match = CHECKS_TOPIC.exec(subject);
  if (!match) return undefined;
  const number = Number(match[3]);
  if (!Number.isSafeInteger(number)) return undefined;
  const repo = `${match[1]}/${match[2]}` as `${string}/${string}`;
  const payload = recordPayload(envelope);
  if (
    !payload ||
    payload.kind !== "checks" ||
    payload.repo !== repo ||
    payload.number !== String(number) ||
    (payload.is_head !== undefined && payload.is_head !== true)
  ) {
    return undefined;
  }
  const sha = stringValue(payload.sha);
  const checkRuns = attemptSet(payload.check_runs);
  const generation =
    typeof payload.generation === "number" &&
    Number.isSafeInteger(payload.generation) &&
    payload.generation >= 0
      ? payload.generation
      : undefined;
  const snapshot = stringValue(payload.snapshot);
  const settledAt =
    payload.settled_at === undefined
      ? envelope.issued_at
      : typeof payload.settled_at === "number" &&
          Number.isSafeInteger(payload.settled_at) &&
          payload.settled_at >= 0
        ? payload.settled_at
        : undefined;
  const failed = statusGroup(payload, "failed");
  const cancelled = statusGroup(payload, "cancelled");
  if (
    !sha ||
    checkRuns === undefined ||
    generation === undefined ||
    !snapshot ||
    settledAt === undefined ||
    !failed ||
    !cancelled
  ) {
    return undefined;
  }
  return {
    repo,
    number,
    sha,
    failed: failed.checks,
    cancelledCount: cancelled.count,
    settledAt,
    checkRuns,
    generation,
    snapshot,
  };
}

/** The payload's attempt set: a non-empty list of `{name, id}` with unique names and positive ids, normalized to name order. */
function attemptSet(value: unknown): CheckRunRef[] | undefined {
  if (!Array.isArray(value) || value.length === 0) return undefined;
  const runs = new Map<string, number>();
  for (const entry of value) {
    const record = asRecord(entry);
    const name = record?.name;
    const id = record?.id;
    if (
      typeof name !== "string" ||
      name === "" ||
      typeof id !== "number" ||
      !Number.isSafeInteger(id) ||
      id <= 0 ||
      runs.has(name)
    ) {
      return undefined;
    }
    runs.set(name, id);
  }
  return sortedCheckRunRefs(runs);
}

function treeFor(state: LegionState, issue: IssueKey): TreeState | undefined {
  let current = issue;
  const visited = new Set<IssueKey>();
  while (!visited.has(current)) {
    visited.add(current);
    const tree = state.trees[current];
    if (tree) return tree;
    const parent = state.issues[current]?.parent;
    if (!parent) return undefined;
    current = parent;
  }
  return undefined;
}

function heldTarget(state: LegionState, token: string): HeldTarget | undefined {
  const parsed = parseRoleToken(state.project, token);
  if (!parsed || "controller" in parsed) return undefined;
  const tree = treeFor(state, parsed.issue);
  if (!tree) return undefined;
  return {
    heldEvents: tree.heldEvents,
    isActive: () => state.issues[parsed.issue]?.released === true && tree.status === "active",
  };
}

function addHeld(
  target: HeldTarget,
  role: string,
  payloadJson: string,
  envelope: EnvelopeJson
): HeldEvent {
  const existing = target.heldEvents.find(
    (held) => held.role === role && held.eventId === envelope.event_id
  );
  if (existing) return existing;
  const held: HeldEvent = {
    role,
    payloadJson,
    heldAt: new Date(envelope.issued_at).toISOString(),
    eventId: envelope.event_id,
  };
  target.heldEvents.push(held);
  return held;
}

function removeHeld(target: HeldTarget, held: HeldEvent): void {
  const index = target.heldEvents.indexOf(held);
  if (index >= 0) target.heldEvents.splice(index, 1);
}

function isMention(subject: string): boolean {
  return (
    (subject.startsWith("notifications.github.") || subject.startsWith("notifications.slack.")) &&
    subject.endsWith(".mention")
  );
}

function exceptionInfo(
  state: LegionState,
  subject: string,
  envelope: EnvelopeJson
): ExceptionInfo | undefined {
  if (!subject.startsWith(EXCEPTION_TOPIC)) return undefined;
  const token = subject.slice(EXCEPTION_TOPIC.length);
  if (!token || token.includes(".")) return undefined;
  const parsed = parseRoleToken(state.project, token);
  if (!parsed) return undefined;
  const payload = recordPayload(envelope);
  const topic = stringValue(payload?.original_topic);
  const eventId = stringValue(payload?.event_id);
  const originalPayload = stringValue(payload?.payload);
  const reason = stringValue(payload?.reason);
  if (
    !topic ||
    !eventId ||
    originalPayload === undefined ||
    (reason !== "no_holder" && reason !== "delivery_failed")
  ) {
    return undefined;
  }
  return {
    roleToken: token,
    reason,
    original: { topic, payload: originalPayload, eventId },
    ...("controller" in parsed ? { controller: true as const } : {}),
  };
}

export interface EventPump {
  applyEffects(effects: Effect[], envelope: EnvelopeJson): Promise<void>;
  redeliverControllerEvents(): Promise<void>;
  publishControllerEvent(payload: { type: string }, envelope: EnvelopeJson): Promise<void>;
  stop(): void;
  drain(): Promise<void>;
}

export function startEventPump(deps: EventPumpDeps): EventPump {
  let stopped = false;
  const retryTimers = new Set<unknown>();
  const pending = new Set<Promise<void>>();
  const failures: unknown[] = [];
  const controllerTarget: HeldTarget = {
    heldEvents: deps.state.controllerHeldEvents,
    isActive: () => true,
  };

  const track = (operation: Promise<void>): void => {
    pending.add(operation);
    void operation.then(
      () => {
        pending.delete(operation);
      },
      (error) => {
        pending.delete(operation);
        failures.push(error);
      }
    );
  };

  const scheduleRetry = (
    role: string,
    payloadJson: string,
    envelope: EnvelopeJson,
    target: HeldTarget | undefined,
    held: HeldEvent | undefined,
    attempt: number
  ): void => {
    if (stopped) return;
    const delay = Math.min(INITIAL_RETRY_DELAY_MS * 2 ** (attempt - 1), MAX_RETRY_DELAY_MS);
    const timer = setTimeout(() => {
      retryTimers.delete(timer);
      void publishRole(role, payloadJson, envelope, target, held, attempt);
    }, delay);
    retryTimers.add(timer);
  };

  const publishRole = async (
    role: string,
    payloadJson: string,
    envelope: EnvelopeJson,
    target: HeldTarget | undefined,
    held: HeldEvent | undefined,
    attempt: number
  ): Promise<void> => {
    if (stopped || (target && !target.isActive())) return;
    try {
      await deps.envoyPublish(roleTopic(role), payloadJson);
    } catch {
      const persisted = target ? (held ?? addHeld(target, role, payloadJson, envelope)) : undefined;
      await deps.saveState();
      scheduleRetry(role, payloadJson, envelope, target, persisted, attempt + 1);
      return;
    }

    if (target && held) removeHeld(target, held);
    await deps.saveState();
  };

  const publishEffect = async (
    role: string,
    payload: LegionEventPayload,
    envelope: EnvelopeJson
  ): Promise<void> => {
    const payloadJson = JSON.stringify(payload);
    const target = heldTarget(deps.state, role);
    if (target && !target.isActive()) {
      addHeld(target, role, payloadJson, envelope);
      return;
    }
    await publishRole(role, payloadJson, envelope, target, undefined, 0);
  };

  const publishController = async (payloadJson: string, envelope: EnvelopeJson): Promise<void> => {
    const role = controllerToken(deps.state.project);
    if (!controllerTarget.isActive()) {
      addHeld(controllerTarget, role, payloadJson, envelope);
      await deps.saveState();
      return;
    }
    await publishRole(role, payloadJson, envelope, controllerTarget, undefined, 0);
  };

  const applyEffects = async (effects: Effect[], envelope: EnvelopeJson): Promise<void> => {
    for (const effect of effects) {
      if (effect.kind === "publish") await publishEffect(effect.role, effect.payload, envelope);
      else if (effect.kind === "controller") {
        await publishController(JSON.stringify(effect.payload), envelope);
      } else if (effect.kind === "linger") {
        await deps.onLinger(effect.tree);
      } else if (effect.kind === "probe") {
        await deps.onProbe(effect.tree);
      } else if (effect.kind === "approval-status") {
        await deps.onApprovalStatus(effect);
      }
    }
  };

  const applyEffectsAndSave = async (effects: Effect[], envelope: EnvelopeJson): Promise<void> => {
    await applyEffects(effects, envelope);
    await deps.saveState();
  };

  const handleChecks = async (subject: string, envelope: EnvelopeJson): Promise<boolean> => {
    const input = checksInput(subject, envelope);
    if (!input) {
      console.debug(
        `[legion] ignored malformed or non-head checks event ${envelope.event_id} subject=${subject}`
      );
      return false;
    }
    const pr = deps.state.prs[`${input.repo}#${input.number}`];
    if (!pr) return false;
    if (pr.headSha !== input.sha) {
      console.debug(
        `[legion] ignored non-head checks event ${envelope.event_id} subject=${subject} sha=${input.sha} head_sha=${pr.headSha}`
      );
      return false;
    }
    const verdict = input.failed.length > 0 ? "red" : input.cancelledCount === 0 ? "green" : null;
    const classification = classifySettlement(pr, { ...input, verdict, failing: input.failed });
    if (classification === "stale") {
      console.debug(
        `[legion] ignored stale checks event ${envelope.event_id} subject=${subject} sha=${input.sha}`
      );
      return false;
    }
    if (classification === "duplicate") return false;
    if (classification === "conflict") {
      console.warn(
        `[legion] conflicting checks settlement ${envelope.event_id} subject=${subject} sha=${input.sha} stored_snapshot=${pr.ciSnapshot ?? "<none>"} incoming_snapshot=${input.snapshot}`
      );
      return false;
    }
    if (classification === "refresh") {
      // Agrees with the verdict a terminal GitHub read holds at this attempt
      // set: the listener identity moves, GitHub's authority stays.
      refreshCiIdentity(pr, input.generation, input.snapshot, input.settledAt);
      return true;
    }
    // A newer attempt set, or a later generation at a set GitHub has not read:
    // the listener's view is authoritative for the names it reports until
    // GitHub reads this set; names it omits keep their last known outcome.
    const outcome = effectiveOutcome(pr, {
      checkRuns: input.checkRuns,
      verdict,
      failing: input.failed,
    });
    writeCiFence(pr, {
      checkRuns: input.checkRuns,
      generation: input.generation,
      snapshot: input.snapshot,
    });
    pr.ciReconciled = false;
    await applyEffects(
      settleCiVerdict(
        deps.state,
        pr,
        {
          verdict: outcome.verdict,
          failing: outcome.failing,
          failingStatuses: pr.failingStatuses,
          settledAt: input.settledAt,
        },
        deps.config
      ),
      envelope
    );
    return true;
  };

  const handleMessage = async (subject: string, data: string): Promise<void> => {
    const envelope = EnvelopeSchema.parse(JSON.parse(data)) as EnvelopeJson;
    let shouldSave = true;
    if (CHECKS_TOPIC.test(subject)) shouldSave = await handleChecks(subject, envelope);
    else if (isMention(subject)) {
      await publishController(
        typeof envelope.payload === "string" ? envelope.payload : "{}",
        envelope
      );
    } else {
      const rawPayload = recordPayload(envelope);
      if (
        subject.startsWith("notifications.github.") &&
        rawPayload &&
        rawPayload.kind === undefined &&
        asRecord(rawPayload.pull_request) &&
        !("review" in rawPayload) &&
        !("comment" in rawPayload)
      ) {
        console.warn(
          "legion: ignored raw-shaped GitHub payload (nested pull_request); Envoy emits kind/action/head_sha"
        );
      } else {
        const exception = exceptionInfo(deps.state, subject, envelope);
        if (exception) {
          if (exception.controller) {
            addHeld(controllerTarget, exception.roleToken, exception.original.payload, {
              ...envelope,
              event_id: exception.original.eventId,
            });
            await deps.saveState();
          }
          await deps.onException(exception);
        } else {
          await applyEffects(
            reduceGithubEvent(deps.state, subject, envelope, deps.config),
            envelope
          );
        }
      }
    }
    if (shouldSave) await deps.saveState();
    console.log(`[legion] consumed event ${envelope.event_id} subject=${subject}`);
  };

  const unsubscribers = [
    deps.nats.subscribe("notifications.github.>", (subject, data) => {
      track(handleMessage(subject, data));
    }),
    deps.nats.subscribe("notifications.slack.*.*.mention", (subject, data) => {
      track(handleMessage(subject, data));
    }),
    deps.nats.subscribe("notifications.envoy.exceptions.notifications.role.>", (subject, data) => {
      track(handleMessage(subject, data));
    }),
  ];

  return {
    applyEffects: applyEffectsAndSave,
    async publishControllerEvent(payload: { type: string }, envelope: EnvelopeJson): Promise<void> {
      await publishController(JSON.stringify(payload), envelope);
    },
    async redeliverControllerEvents(): Promise<void> {
      for (const held of [...deps.state.controllerHeldEvents]) {
        const heldAt = Date.parse(held.heldAt);
        await publishRole(
          controllerToken(deps.state.project),
          held.payloadJson,
          {
            event_id: held.eventId,
            issued_at: Number.isNaN(heldAt) ? Date.now() : heldAt,
          },
          controllerTarget,
          held,
          0
        );
      }
    },
    async drain(): Promise<void> {
      while (pending.size > 0) await Promise.allSettled([...pending]);
      if (failures.length > 0) throw new AggregateError(failures, "Event pump processing failed");
    },
    stop(): void {
      stopped = true;
      for (const unsubscribe of unsubscribers) unsubscribe();
      for (const timer of retryTimers) clearTimeout(timer as never);
      retryTimers.clear();
    },
  };
}

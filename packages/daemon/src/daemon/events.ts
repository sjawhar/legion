import {
  controllerToken,
  EnvelopeSchema,
  formatIssueKey,
  type IssueKey,
  parseRoleToken,
  roleToken,
  roleTopic,
} from "@legion/contracts";
import type { DaemonConfig } from "./config";
import type { HeldEvent, LegionState, PrState, TreeState } from "./legion-state";
import {
  type Effect,
  type EnvelopeJson,
  type LegionEventPayload,
  reduceCiEmission,
  reduceGithubEvent,
  type CiEmission as SettledCiEmission,
} from "./reducers";

const CHECKS_TOPIC = /^notifications\.github\.([^.]+)\.([^.]+)\.pr\.(\d+)\.checks$/;
const EXCEPTION_TOPIC = "notifications.envoy.exceptions.notifications.role.";
const MAX_RETRY_DELAY_MS = 30_000;
const INITIAL_RETRY_DELAY_MS = 1_000;

type JsonRecord = Record<string, unknown>;

type CiEmission = SettledCiEmission | { type: "ci-first-red"; check: string; sha: string };

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
  return asRecord(JSON.parse(envelope.payload));
}

function statusGroup(payload: JsonRecord, key: string): { count: number; checks: string[] } {
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
    throw new Error(`Invalid checks payload ${key} group`);
  }
  return { count, checks };
}

function checksInput(subject: string, envelope: EnvelopeJson): ChecksInput | undefined {
  const match = CHECKS_TOPIC.exec(subject);
  if (!match) return undefined;
  const number = Number(match[3]);
  if (!Number.isSafeInteger(number)) throw new Error(`Invalid checks topic: ${subject}`);
  const repo = `${match[1]}/${match[2]}` as `${string}/${string}`;
  const payload = recordPayload(envelope);
  if (!payload) throw new Error("Checks envelope requires a JSON object payload");
  if (
    payload.kind !== "checks" ||
    payload.repo !== repo ||
    payload.number !== String(number) ||
    payload.is_head !== true
  ) {
    throw new Error(`Checks envelope does not match subject: ${subject}`);
  }
  const sha = stringValue(payload.sha);
  if (!sha) throw new Error("Checks envelope requires sha");
  const failed = statusGroup(payload, "failed");
  const cancelled = statusGroup(payload, "cancelled");
  return { repo, number, sha, failed: failed.checks, cancelledCount: cancelled.count };
}

function issueForBranch(repo: string, branch: string): IssueKey | undefined {
  const match = /^legion\/issue-(\d+)$/.exec(branch);
  if (!match) return undefined;
  const [owner, name, ...extra] = repo.split("/");
  const number = Number(match[1]);
  if (!owner || !name || extra.length > 0 || !Number.isSafeInteger(number)) return undefined;
  return formatIssueKey(owner, name, number);
}

function findOrCreatePr(state: LegionState, input: ChecksInput): PrState | undefined {
  const prKey = `${input.repo}#${input.number}`;
  const existing = state.prs[prKey];
  if (existing) return existing;
  const mapping = Object.entries(state.prByBranch).find(
    ([branchKey, mappedPrKey]) => mappedPrKey === prKey && branchKey.startsWith(`${input.repo}@`)
  );
  const branch = mapping?.[0].slice(input.repo.length + 1);
  if (!branch) return undefined;

  const mappedPrKey = state.prByBranch[`${input.repo}@${branch}`];
  if (mappedPrKey !== prKey) return undefined;
  const issue = issueForBranch(input.repo, branch);
  if (!issue || !state.issues[issue]) return undefined;

  const pr: PrState = {
    key: issue,
    repo: input.repo,
    number: input.number,
    headSha: input.sha,
    firstRedEmitted: false,
    settledRedEmitted: false,
    greenEmitted: false,
    lastEventAt: 0,
    fixAttempts: 0,
  };
  state.prs[prKey] = pr;
  return pr;
}

function updatePrHead(pr: PrState, sha: string): void {
  if (pr.headSha === sha) return;
  if (pr.settledRedEmitted) pr.fixAttempts += 1;
  pr.headSha = sha;
  pr.firstRedEmitted = false;
  pr.settledRedEmitted = false;
  pr.greenEmitted = false;
  delete pr.reviewDecision;
}

function ciEmissions(pr: PrState, input: ChecksInput): CiEmission[] {
  if (input.failed.length > 0) {
    const emissions: CiEmission[] = [];
    if (!pr.firstRedEmitted) {
      pr.firstRedEmitted = true;
      emissions.push({ type: "ci-first-red", check: input.failed[0], sha: pr.headSha });
    }
    if (!pr.settledRedEmitted) {
      pr.settledRedEmitted = true;
      emissions.push({ type: "ci-settled-red", failing: input.failed, sha: pr.headSha });
    }
    return emissions;
  }
  if (input.cancelledCount === 0 && !pr.greenEmitted) {
    pr.greenEmitted = true;
    return [{ type: "ci-green", sha: pr.headSha }];
  }
  return [];
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

  const publishCiEmissions = async (
    pr: PrState,
    emissions: CiEmission[],
    envelope: EnvelopeJson
  ): Promise<void> => {
    const role = roleToken(deps.state.project, pr.key, "implementer");
    for (const emission of emissions) {
      await publishEffect(role, emission, envelope);
      if (emission.type !== "ci-first-red") {
        await applyEffects(
          reduceCiEmission(deps.state, pr.repo, pr.number, emission, deps.config),
          envelope
        );
      }
    }
  };

  const handleChecks = async (subject: string, envelope: EnvelopeJson): Promise<void> => {
    const input = checksInput(subject, envelope);
    if (!input) return;
    const pr = findOrCreatePr(deps.state, input);
    if (!pr) return;
    updatePrHead(pr, input.sha);
    pr.lastEventAt = envelope.issued_at;
    await publishCiEmissions(pr, ciEmissions(pr, input), envelope);
  };

  const handleMessage = async (subject: string, data: string): Promise<void> => {
    const envelope = EnvelopeSchema.parse(JSON.parse(data)) as EnvelopeJson;
    if (CHECKS_TOPIC.test(subject)) await handleChecks(subject, envelope);
    else if (isMention(subject)) {
      await publishController(
        typeof envelope.payload === "string" ? envelope.payload : "{}",
        envelope
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
        await applyEffects(reduceGithubEvent(deps.state, subject, envelope, deps.config), envelope);
      }
    }
    await deps.saveState();
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

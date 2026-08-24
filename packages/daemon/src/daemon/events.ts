import {
  controllerToken,
  EnvelopeSchema,
  formatIssueKey,
  type IssueKey,
  parseRoleToken,
  roleToken,
  roleTopic,
} from "@legion/contracts";
import { type CheckObservation, type CiEmission, reduceCheck, settle } from "./ci-reducer";
import type { DaemonConfig } from "./config";
import type { HeldEvent, LegionState, PrState, TreeState } from "./legion-state";
import {
  type Effect,
  type EnvelopeJson,
  type LegionEventPayload,
  reduceCiEmission,
  reduceGithubEvent,
} from "./reducers";

const CHECK_TOPIC = /^notifications\.github\.([^.]+)\.([^.]+)\.pr\.(\d+)\.check(?:\.|$)/;
const EXCEPTION_TOPIC = "notifications.envoy.exceptions.notifications.role.";
const SETTLE_INTERVAL_MS = 5_000;
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

interface CheckInput {
  repo: `${string}/${string}`;
  number: number;
  branch?: string;
  observation: CheckObservation;
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

function valueFrom(records: Array<JsonRecord | undefined>, key: string): unknown {
  for (const record of records) {
    if (record?.[key] !== undefined) return record[key];
  }
  return undefined;
}

function checkInput(subject: string, envelope: EnvelopeJson): CheckInput | undefined {
  const match = CHECK_TOPIC.exec(subject);
  if (!match) return undefined;
  const number = Number(match[3]);
  if (!Number.isSafeInteger(number)) return undefined;
  const repo = `${match[1]}/${match[2]}` as `${string}/${string}`;
  const payload = recordPayload(envelope);
  if (!payload) return undefined;
  const checkRun = asRecord(payload.check_run);
  const checkSuite = asRecord(payload.check_suite);
  const nestedSuite = asRecord(checkRun?.check_suite);
  const records = [payload, checkRun, checkSuite, nestedSuite];
  const sha = stringValue(valueFrom(records, "sha")) ?? stringValue(valueFrom(records, "head_sha"));
  const name =
    stringValue(valueFrom(records, "name")) ??
    stringValue(asRecord(checkSuite?.app)?.name) ??
    stringValue(asRecord(nestedSuite?.app)?.name);
  const status = stringValue(valueFrom(records, "status"));
  const rawConclusion = valueFrom(records, "conclusion");
  const conclusion =
    rawConclusion === null || typeof rawConclusion === "string" ? rawConclusion : undefined;
  const branch =
    stringValue(valueFrom(records, "branch")) ?? stringValue(valueFrom(records, "head_branch"));
  if (!sha || !name || !status || conclusion === undefined) return undefined;

  return {
    repo,
    number,
    branch,
    observation: { sha, name, status, conclusion },
  };
}

function issueForBranch(repo: string, branch: string): IssueKey | undefined {
  const match = /^legion\/issue-(\d+)$/.exec(branch);
  if (!match) return undefined;
  const [owner, name, ...extra] = repo.split("/");
  const number = Number(match[1]);
  if (!owner || !name || extra.length > 0 || !Number.isSafeInteger(number)) return undefined;
  return formatIssueKey(owner, name, number);
}

function findOrCreatePr(state: LegionState, input: CheckInput): PrState | undefined {
  const prKey = `${input.repo}#${input.number}`;
  const existing = state.prs[prKey];
  if (existing) return existing;
  let branch = input.branch;
  if (!branch) {
    const mapping = Object.entries(state.prByBranch).find(
      ([branchKey, mappedPrKey]) => mappedPrKey === prKey && branchKey.startsWith(`${input.repo}@`)
    );
    branch = mapping?.[0].slice(input.repo.length + 1);
  }
  if (!branch) return undefined;

  const mappedPrKey = state.prByBranch[`${input.repo}@${branch}`];
  if (mappedPrKey !== undefined && mappedPrKey !== prKey) return undefined;
  const issue = issueForBranch(input.repo, branch);
  if (!issue || !state.issues[issue]) return undefined;

  const pr: PrState = {
    key: issue,
    repo: input.repo,
    number: input.number,
    headSha: input.observation.sha,
    checks: {},
    firstRedEmitted: false,
    settledRedEmitted: false,
    greenEmitted: false,
    lastEventAt: 0,
    fixAttempts: 0,
  };
  state.prs[prKey] = pr;
  state.prByBranch[`${input.repo}@${branch}`] = prKey;
  return pr;
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

  const handleCheck = async (subject: string, envelope: EnvelopeJson): Promise<void> => {
    const input = checkInput(subject, envelope);
    if (!input) return;
    const pr = findOrCreatePr(deps.state, input);
    if (!pr) return;
    const now = Date.now();
    reduceCheck(pr, input.observation, now);
    await publishCiEmissions(pr, settle(pr, now, 0), envelope);
  };

  const settleChecks = async (): Promise<void> => {
    const now = Date.now();
    for (const pr of Object.values(deps.state.prs)) {
      const envelope: EnvelopeJson = {
        event_id: `ci:${pr.repo}#${pr.number}:${pr.headSha}`,
        issued_at: now,
      };
      await publishCiEmissions(pr, settle(pr, now, deps.config.ciQuietMs), envelope);
    }
    await deps.saveState();
  };

  const handleMessage = async (subject: string, data: string): Promise<void> => {
    const envelope = EnvelopeSchema.parse(JSON.parse(data)) as EnvelopeJson;
    if (CHECK_TOPIC.test(subject)) await handleCheck(subject, envelope);
    else if (isMention(subject)) {
      await publishController(
        typeof envelope.payload === "string" ? envelope.payload : "{}",
        envelope
      );
    } else {
      const exception = exceptionInfo(deps.state, subject, envelope);
      if (exception) await deps.onException(exception);
      else
        await applyEffects(reduceGithubEvent(deps.state, subject, envelope, deps.config), envelope);
    }
    await deps.saveState();
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
  const sweepTimer = setInterval(() => {
    track(settleChecks());
  }, SETTLE_INTERVAL_MS);

  return {
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
      clearInterval(sweepTimer);
      for (const timer of retryTimers) clearTimeout(timer as never);
      retryTimers.clear();
    },
  };
}

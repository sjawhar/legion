import {
  controllerToken,
  EnvelopeSchema,
  type IssueKey,
  parseRoleToken,
  roleTopic,
} from "@legion/contracts";
import { type CheckRunRef, sortedCheckRunRefs } from "../state/types";
import { createCancellableSleep } from "./cancellable-sleep";
import type { DaemonConfig } from "./config";
import type { LegionState } from "./legion-state";
import type { DurableMessageControl, NatsTransport } from "./nats-transport";
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
/** JetStream stream carrying durable notifications; mirrors packages/envoy/internal/bus/nats.go:18. */
const GITHUB_STREAM = "ENVOY_NOTIFICATIONS";
/** Fixed nak delay for a durable delivery that fails for a reason that may be transient (see processDurableMessage). */
const DURABLE_NAK_DELAY_MS = 30_000;

/** Bounds a JetStream `term` reason so an oversized reducer/parse error never fails the term frame itself. */
const MAX_TERM_REASON_BYTES = 1_024;
const TERM_REASON_ELLIPSIS = "…";
const textEncoder = new TextEncoder();

/** Truncates `reason` so its UTF-8 byte length, including the appended ellipsis, never exceeds `MAX_TERM_REASON_BYTES`. */
export function truncateTermReason(reason: string): string {
  if (textEncoder.encode(reason).length <= MAX_TERM_REASON_BYTES) return reason;
  const budget = MAX_TERM_REASON_BYTES - textEncoder.encode(TERM_REASON_ELLIPSIS).length;
  let truncated = reason.slice(0, budget);
  while (textEncoder.encode(truncated).length > budget) truncated = truncated.slice(0, -1);
  return `${truncated}${TERM_REASON_ELLIPSIS}`;
}

/** Logs a poison durable message (never redeliverable) and terminates it so JetStream never retries it. */
function poisonMessage(
  subject: string,
  control: DurableMessageControl,
  reason: string,
  eventId?: string
): void {
  const truncatedReason = truncateTermReason(reason);
  console.error(
    `[legion] poison durable message on ${subject} (stream_seq=${control.streamSequence} delivery_seq=${control.deliverySequence}${eventId ? ` event_id=${eventId}` : ""}): ${truncatedReason}`
  );
  control.term(truncatedReason);
}

/**
 * Wraps a synchronous reducer's throw so `processDurableMessage` can tell it
 * apart from every other durable-lane failure (a rejected `saveState`, a
 * rejected publish): reducers are pure and deterministic, so a throw here
 * will throw identically on every redelivery — it is poison, not a
 * transient condition, and gets termed with a loud log instead of nak'd.
 */
class DurableReducerFailure extends Error {
  constructor(cause: unknown) {
    super(cause instanceof Error ? cause.message : String(cause));
    this.name = "DurableReducerFailure";
  }
}

/**
 * Wraps a failure from a reducer-derived event's effect dispatch (a
 * non-404 publish/controller rejection, or an `onLinger`/`onProbe`/
 * `onApprovalStatus` handler throwing) or its `saveState` — anything
 * `applyDurableEvent` hits after the reducer has already mutated live
 * state. Distinguishes this from `DurableReducerFailure` (poison, no
 * mutation risk) and from a GitHub mention's publish failure (no
 * reducer, so no mutation risk either): only this class means memory may
 * be dirty, and `processDurableMessage` responds by going fatal instead
 * of nak'ing.
 */
class DurableFatalFailure extends Error {
  constructor(cause: unknown) {
    super(cause instanceof Error ? cause.message : String(cause));
    this.name = "DurableFatalFailure";
  }
}

type JsonRecord = Record<string, unknown>;

export interface UndeliverableInfo {
  role: string;
  eventId: string;
  envelope: EnvelopeJson;
  subject: string;
  /** `envelope.payload_summary`, surfaced directly so a hook doesn't have to re-derive it. */
  summary?: string;
  kind: "publish" | "controller";
}

export interface EventPumpDeps {
  nats: Pick<NatsTransport, "subscribe" | "consumeDurable" | "publish" | "flush">;
  envoyPublish(topic: string, payloadJson: string): Promise<void>;
  state: LegionState;
  saveState(): Promise<void>;
  onException(ex: ExceptionInfo): Promise<void>;
  onLinger(tree: IssueKey): Promise<void>;
  onProbe(tree: IssueKey): Promise<void>;
  onApprovalStatus(effect: Extract<Effect, { kind: "approval-status" }>): Promise<void>;
  /**
   * Called when a durable effect's role has no live holder (Envoy 404).
   * State is already durable and correct by this point — the effect is a
   * wake the role missed, and the worker's own catch-up on ready/resume
   * (see catchup.ts) recovers it from current state, not from replaying
   * this event. Defaults to a log line naming the role and event id.
   */
  onUndeliverable?(info: UndeliverableInfo): void | Promise<void>;
  /**
   * Called when a durable message's effect dispatch or `saveState` fails
   * after its reducer has already mutated live state (`DurableFatalFailure`
   * — see `applyDurableEvent`) or when a reducer itself throws
   * (`DurableReducerFailure`, after the poison log/term). Memory may now
   * be dirty, so the process must not keep serving other durable messages
   * against it. Defaults to `process.exit(1)`: the daemon's supervisor is
   * expected to restart it (see AGENTS.md), reloading state from the last
   * successful save, so JetStream's redelivery of the still-unacked
   * message runs against clean state.
   */
  fatal?(error: unknown): void | Promise<void>;
  /** Overridable for tests; the real bounded backoff (`drainControllerNoticesLocked`'s doc
   * comment) is 1s doubling to a 60s ceiling, which no test should have to wait out for real. */
  controllerNoticeRetryDelayMs?(attempt: number): number;
  config: DaemonConfig;
}

export interface ExceptionInfo {
  roleToken: string;
  reason: "no_holder" | "delivery_failed";
  original: { topic: string; payload: string; eventId: string };
  controller?: true;
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
    payload?.kind !== "checks" ||
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

interface EffectPublisher {
  publishRole(role: string, payload: LegionEventPayload): Promise<void>;
  publishController(payload: LegionEventPayload): Promise<void>;
}

function isSlackMention(subject: string): boolean {
  return subject.startsWith("notifications.slack.") && subject.endsWith(".mention");
}

function isGithubMention(subject: string): boolean {
  return subject.startsWith("notifications.github.") && subject.endsWith(".mention");
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
  /**
   * Runs `fn` serialized against every durable GitHub message: enqueued
   * after every earlier operation on this queue settles, and before any
   * later one starts. Used to run resync exclusively of the durable lane
   * (see the queue's doc comment in events.ts) — not for ordinary event
   * handling, which already goes through the queue internally.
   */
  runExclusive<T>(fn: () => Promise<T>): Promise<T>;
  publishControllerEvent(payload: { type: string }, envelope: EnvelopeJson): Promise<void>;
  /**
   * Delivers every `controllerPendingNotices` entry, in order, to the controller's role topic —
   * called once the controller has just claimed its role (`/controller/ready`) and once more at
   * boot if a controller role claim was already live when the daemon started (see `index.ts`'s
   * boot sequence), so the publish should now succeed. Removes each notice, by identity, only
   * after Envoy acks its publication (at-most-once, same discipline the deleted held-event queue
   * once used for its own redelivery). A failed publish retries the same notice with a bounded
   * backoff (1s doubling to a 60s ceiling) rather than giving up after one attempt; the retry
   * loop stops only once the queue empties or the pump itself stops (`stop()` cancels any wait in
   * progress). Serialized: two overlapping calls (a second `/controller/ready` arriving while the
   * first drain, including any retry backoff, is still in progress) run one at a time, never
   * interleaved, so neither double-publishes an entry nor drops one to a race against the other's
   * removal.
   */
  drainControllerNotices(): Promise<void>;
  stop(): void;
  drain(): Promise<void>;
}

export function startEventPump(deps: EventPumpDeps): EventPump {
  const pending = new Set<Promise<void>>();
  const failures: unknown[] = [];

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

  /** True for the "no holder for role X" 404 Envoy's publish endpoint returns before a session claims the role — the normal state before one exists, never a broken publish path. */
  function isNoHolderError(error: unknown): boolean {
    return (
      typeof error === "object" &&
      error !== null &&
      "status" in error &&
      (error as { status?: unknown }).status === 404
    );
  }

  /**
   * Publishes a GitHub mention directly through Envoy, propagating any
   * rejection (including a 404 no holder) so the durable message naks and
   * retries: unlike a reducer-derived effect, a mention has no persisted
   * state to fall back on if it's lost — this is the only durability
   * mechanism it gets. Failure logging happens once, in
   * `processDurableMessage`'s catch (which already distinguishes a
   * no-holder 404 from every other failure) — not here.
   */
  const publishRoleDirect = (role: string, payloadJson: string): Promise<void> =>
    deps.envoyPublish(roleTopic(role), payloadJson);

  const runRecovery = async (info: UndeliverableInfo): Promise<void> => {
    if (deps.onUndeliverable) {
      await deps.onUndeliverable(info);
      return;
    }
    console.error(
      `legion: no holder for ${info.role}, event ${info.eventId} undelivered (subject=${info.subject}${info.envelope.payload_summary ? ` summary=${info.envelope.payload_summary}` : ""} effect=${info.kind}); recovered via the worker's own catch-up on resume`
    );
  };

  /**
   * Records (or immediately fires) a 404 no-holder notice. `recoveries`, supplied only by
   * `applyDurableEvent`, defers the call: pushing the info instead of running it keeps the
   * durable transaction's dispatch-then-save a single state mutation, since `onUndeliverable`
   * itself resumes a worker and saves state. Every other caller (the local lane, direct
   * controller publishes) has no such transaction to protect and runs it inline.
   */
  const notifyUndeliverable = async (
    role: string,
    eventId: string,
    envelope: EnvelopeJson,
    subject: string,
    kind: "publish" | "controller",
    recoveries?: UndeliverableInfo[]
  ): Promise<void> => {
    const info: UndeliverableInfo = {
      role,
      eventId,
      envelope,
      subject,
      summary: envelope.payload_summary,
      kind,
    };
    if (recoveries) {
      recoveries.push(info);
      return;
    }
    await runRecovery(info);
  };

  /**
   * Publishes an effect's role/controller payload through Envoy. A 404 "no holder" is handled:
   * outside a durable transaction (`recoveries` omitted) it calls `deps.onUndeliverable`
   * immediately - the everyday state before a role's session claims it (or after its worker
   * died), recovered by that worker's own catch-up on resume, never by holding or retrying the
   * event. Inside one (`recoveries` supplied by `applyDurableEvent`) it only records the info;
   * the recovery itself runs once, best-effort, after that transaction's own save commits (see
   * `applyDurableEvent`). Anything else propagates: for the durable GitHub lane the caller's
   * dispatch-then-save transaction treats it as fatal (this effect is not yet durable, and
   * continuing with a live-state mutation whose full effect set didn't get applied would leave
   * dirty memory serving other events); for a local caller (resync, or any other effect
   * dispatched outside the durable lane) it simply fails that caller's own request. Shared by
   * both lanes - there is no separate held/retry path.
   */
  const publisher = (
    eventId: string,
    envelope: EnvelopeJson,
    subject: string,
    recoveries?: UndeliverableInfo[]
  ): EffectPublisher => {
    const publishOrThrow = async (
      role: string,
      payloadJson: string,
      kind: "publish" | "controller"
    ): Promise<void> => {
      try {
        await deps.envoyPublish(roleTopic(role), payloadJson);
      } catch (error) {
        if (isNoHolderError(error)) {
          await notifyUndeliverable(role, eventId, envelope, subject, kind, recoveries);
          return;
        }
        throw error;
      }
    };
    return {
      publishRole: (role, payload) => publishOrThrow(role, JSON.stringify(payload), "publish"),
      publishController: (payload) =>
        publishOrThrow(controllerToken(deps.state.project), JSON.stringify(payload), "controller"),
    };
  };

  /** Publishes directly to the controller role outside any effect dispatch (a Slack mention, or
   * `publishControllerEvent`'s resync/API-triggered controller wake). Unlike `publisher`'s own
   * no-holder handling — every one of *its* effects is reducer-derived and fully recoverable
   * from current state, so nothing needs to survive the 404 itself — a Slack mention's specific
   * text has no other source of truth (see `ControllerPendingNotice`'s doc comment): it is
   * recorded into durable state and saved BEFORE `ensureController` even runs, so a crash
   * between this 404 and the controller's own claim can never lose it. (The resync/API caller's
   * own payload is redundant with the fresh resync `onControllerReady` already re-emits on
   * ready, so recording it too is a harmless no-op, not a behavior change worth special-casing
   * away.) Drained, in order, exactly once each, by `drainControllerNotices` on
   * `/controller/ready`. */
  const publishControllerDirect = async (
    payloadJson: string,
    envelope: EnvelopeJson
  ): Promise<void> => {
    const role = controllerToken(deps.state.project);
    try {
      await deps.envoyPublish(roleTopic(role), payloadJson);
    } catch (error) {
      if (isNoHolderError(error)) {
        deps.state.controllerPendingNotices.push({
          payloadJson,
          eventId: envelope.event_id,
        });
        await deps.saveState();
        await notifyUndeliverable(
          role,
          envelope.event_id,
          envelope,
          "controller-event",
          "controller"
        );
        return;
      }
      throw error;
    }
  };

  // Serializes `drainControllerNotices` the same way `githubQueue` serializes durable GitHub
  // messages/resync -- a dedicated queue, not that one, since this drain never touches a
  // `PrState` or anything `githubQueue` protects and chaining onto it would add unrelated
  // cross-blocking. Two overlapping `/controller/ready` calls are a real possibility (nothing
  // prevents a second controller-ready delivery while the first drain is still awaiting a
  // publish); without this, interleaved reads of the same head entry would publish it twice, and
  // removing by position rather than identity would then let a second entry queued mid-drain
  // lose its own publish to whatever `shift()` happens to remove by the time each call gets
  // there. `.then(() => {}, () => {})` (mirroring `runExclusive`) keeps the queue itself always
  // resolving so one caller's failure never wedges every later drain behind a permanently
  // rejected chain link.
  let controllerNoticeQueue: Promise<void> = Promise.resolve();
  // Set once `stop()` runs; checked by the retry loop below so a sustained Envoy outage never
  // keeps retrying past the pump's own shutdown, and cancels any wait currently in flight so the
  // loop wakes immediately instead of lingering for the rest of its current backoff.
  let controllerDrainDisposed = false;
  const controllerDrainSleep = createCancellableSleep();

  const defaultControllerNoticeRetryDelayMs = (attempt: number): number =>
    Math.min(1_000 * 2 ** attempt, 60_000);

  const drainControllerNoticesLocked = async (): Promise<void> => {
    const role = controllerToken(deps.state.project);
    // Bounded backoff for a retried publish, doubling from 1s to a 60s ceiling by default -- the
    // same doubling-to-a-ceiling shape as `runWithRestart`'s own durable-consumer backoff
    // (nats-transport.ts), just faster off the mark since a stuck retry here blocks nothing else
    // (no reconnect storm to pace against). Overridable via `deps.controllerNoticeRetryDelayMs`
    // so a test never has to wait out a real 1s-plus backoff.
    const retryDelayMs = deps.controllerNoticeRetryDelayMs ?? defaultControllerNoticeRetryDelayMs;
    let attempt = 0;
    while (deps.state.controllerPendingNotices.length > 0 && !controllerDrainDisposed) {
      const notice = deps.state.controllerPendingNotices[0];
      if (!notice) break;
      try {
        await deps.envoyPublish(roleTopic(role), notice.payloadJson);
      } catch (error) {
        // Checked immediately on catching, before anything else: `stop()` racing this exact
        // await (published just as `stop()` ran, then rejected anyway) must never schedule a
        // recovery call or a sleep after the pump has already decided to shut down -- either
        // one would create a timer/call that outlives the pump with nothing left to observe it.
        if (controllerDrainDisposed) return;
        if (isNoHolderError(error)) {
          // No controller is currently claiming the role at all: retrying the publish alone can
          // never succeed until something spawns one. Routes through the same controller
          // recovery every other no-holder 404 does (`ensureController`, via `onUndeliverable`)
          // on every failure, not only once some ceiling is reached -- `ensureController` itself
          // is a no-op once a controller is already live or already being spawned, so calling it
          // repeatedly here is safe.
          await notifyUndeliverable(
            role,
            notice.eventId,
            { event_id: notice.eventId, issued_at: Date.now() },
            "controller-notice-drain",
            "controller"
          );
          // Re-checked: `stop()` racing this exact recovery call (already in flight when it
          // ran) must not arm a sleep afterward either -- the same rule as the check just above,
          // applied again after this await, the loop's only other one before the sleep.
          if (controllerDrainDisposed) return;
        }
        const delayMs = retryDelayMs(attempt);
        console.error(
          `[legion] failed to deliver pending controller notice ${notice.eventId}, retrying in ${delayMs}ms:`,
          error
        );
        attempt += 1;
        await controllerDrainSleep.sleep(delayMs);
        continue;
      }
      attempt = 0;
      // By identity, not position: the entry just published is exactly this `notice` object,
      // wherever it now sits in the array (an unrelated append while this publish was in flight
      // would otherwise make a positional `shift()` remove the wrong entry).
      const index = deps.state.controllerPendingNotices.indexOf(notice);
      if (index !== -1) deps.state.controllerPendingNotices.splice(index, 1);
      await deps.saveState();
    }
  };

  /** The single effect-application switch, shared by every lane; an unrecognized kind crashes loud instead of silently doing nothing. */
  const dispatch = async (effects: Effect[], effectPublisher: EffectPublisher): Promise<void> => {
    for (const effect of effects) {
      if (effect.kind === "publish") await effectPublisher.publishRole(effect.role, effect.payload);
      else if (effect.kind === "controller")
        await effectPublisher.publishController(effect.payload);
      else if (effect.kind === "linger") await deps.onLinger(effect.tree);
      else if (effect.kind === "probe") await deps.onProbe(effect.tree);
      else if (effect.kind === "approval-status") await deps.onApprovalStatus(effect);
      else {
        const unhandled: never = effect;
        throw new Error(
          `[legion] pump received an unhandled effect kind: ${JSON.stringify(unhandled)}`
        );
      }
    }
  };

  const applyEffectsAndSave = async (effects: Effect[], envelope: EnvelopeJson): Promise<void> => {
    await dispatch(effects, publisher(envelope.event_id, envelope, "local"));
    await deps.saveState();
  };

  /**
   * Runs a github-sourced reducer once, directly against the live state
   * (mutating it in place, same as always), dispatches every derived
   * effect, then durably saves. This is the whole transaction, and its
   * order is deliberate: dispatching before saving means every effect
   * (publish/controller/linger/probe/approval-status) has already run by
   * the time state is marked durable, so a crash or failure anywhere in
   * this function can never leave a "saved but not yet woken" or "acked
   * but not yet dispatched" gap. State is the source of truth once this
   * function returns; the caller (processDurableMessage) acks only then.
   *
   * - The reducer throwing is poison, not transient: reducers are
   *   synchronous and pure, so the same throw happens on every redelivery
   *   (see `DurableReducerFailure`, thrown here and termed by the caller).
   * - Any other failure here (a non-404 effect dispatch, a rejected
   *   `saveState`) is fatal (`DurableFatalFailure`): the reducer has
   *   already mutated live state in memory, and that mutation cannot be
   *   trusted to keep serving other messages once part of its effect set
   *   or its save has failed. The caller does not ack or nak; it calls
   *   `deps.fatal` to exit the process, so the supervisor restarts it,
   *   state reloads from the last successful save, and JetStream
   *   redelivers this still-unacked message against that clean state.
   * - A 404 no-holder effect never reaches the catch above: `publisher`
   *   (given `recoveries` here) only records it. Those recoveries run
   *   after `saveState` succeeds, one call each, best-effort - a
   *   recovery itself resumes a worker and saves state, and running it
   *   before this transaction's own save would let a second, unrelated
   *   save land mid-transaction (see `notifyUndeliverable`'s doc
   *   comment); a recovery failing here must not undo or fail an
   *   already-durable event.
   */
  const applyDurableEvent = async (
    subject: string,
    envelope: EnvelopeJson,
    reduce: (state: LegionState) => Effect[]
  ): Promise<void> => {
    let effects: Effect[];
    try {
      effects = reduce(deps.state);
    } catch (error) {
      throw new DurableReducerFailure(error);
    }
    const recoveries: UndeliverableInfo[] = [];
    try {
      await dispatch(effects, publisher(envelope.event_id, envelope, subject, recoveries));
      await deps.saveState();
    } catch (error) {
      throw new DurableFatalFailure(error);
    }
    for (const info of recoveries) {
      try {
        await runRecovery(info);
      } catch (error) {
        console.error(`[legion] onUndeliverable recovery failed for ${info.role}:`, error);
      }
    }
  };

  const handleChecks = async (subject: string, envelope: EnvelopeJson): Promise<void> => {
    const input = checksInput(subject, envelope);
    if (!input) {
      console.debug(
        `[legion] ignored malformed or non-head checks event ${envelope.event_id} subject=${subject}`
      );
      return;
    }
    const pr = deps.state.prs[`${input.repo}#${input.number}`];
    if (!pr) return;
    if (pr.headSha !== input.sha) {
      console.debug(
        `[legion] ignored non-head checks event ${envelope.event_id} subject=${subject} sha=${input.sha} head_sha=${pr.headSha}`
      );
      return;
    }
    const verdict = input.failed.length > 0 ? "red" : input.cancelledCount === 0 ? "green" : null;
    const classification = classifySettlement(pr, { ...input, verdict, failing: input.failed });
    if (classification === "stale") {
      console.debug(
        `[legion] ignored stale checks event ${envelope.event_id} subject=${subject} sha=${input.sha}`
      );
      return;
    }
    if (classification === "duplicate") return;
    if (classification === "conflict") {
      console.warn(
        `[legion] conflicting checks settlement ${envelope.event_id} subject=${subject} sha=${input.sha} stored_snapshot=${pr.ciSnapshot ?? "<none>"} incoming_snapshot=${input.snapshot}`
      );
      return;
    }
    if (classification === "refresh") {
      // Agrees with the verdict a terminal GitHub read holds at this attempt
      // set: the listener identity moves, GitHub's authority stays.
      await applyDurableEvent(subject, envelope, () => {
        refreshCiIdentity(pr, input.generation, input.snapshot, input.settledAt);
        return [];
      });
      return;
    }
    // A newer attempt set, or a later generation at a set GitHub has not read:
    // the listener's view is authoritative for the names it reports until
    // GitHub reads this set; names it omits keep their last known outcome.
    const outcome = effectiveOutcome(pr, {
      checkRuns: input.checkRuns,
      verdict,
      failing: input.failed,
    });
    await applyDurableEvent(subject, envelope, (state) => {
      writeCiFence(pr, {
        checkRuns: input.checkRuns,
        generation: input.generation,
        snapshot: input.snapshot,
      });
      pr.ciReconciled = false;
      return settleCiVerdict(
        state,
        pr,
        { ...outcome, settledAt: input.settledAt },
        deps.config,
        envelope
      );
    });
  };

  const handleEnvelope = async (subject: string, envelope: EnvelopeJson): Promise<void> => {
    if (CHECKS_TOPIC.test(subject)) {
      await handleChecks(subject, envelope);
    } else if (isSlackMention(subject)) {
      await publishControllerDirect(
        typeof envelope.payload === "string" ? envelope.payload : "{}",
        envelope
      );
    } else if (isGithubMention(subject)) {
      // A GitHub mention has no reducer or state to fall back on if it's
      // lost — unlike every other durable effect, it keeps the
      // publish-and-nak-on-failure contract (including on a 404 no
      // holder): there is nothing else that will ever re-derive it.
      await publishRoleDirect(
        controllerToken(deps.state.project),
        typeof envelope.payload === "string" ? envelope.payload : "{}"
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
          await deps.onException(exception);
        } else {
          await applyDurableEvent(subject, envelope, (state) =>
            reduceGithubEvent(state, subject, envelope, deps.config)
          );
        }
      }
    }
    console.log(`[legion] consumed event ${envelope.event_id} subject=${subject}`);
  };

  const handleMessage = async (subject: string, data: string): Promise<void> => {
    const envelope = EnvelopeSchema.parse(JSON.parse(data)) as EnvelopeJson;
    await handleEnvelope(subject, envelope);
  };

  const githubDurable = `legion-${deps.config.project}-github`;
  const githubFilterSubjects = deps.config.repos.map((repo) => {
    const [owner, name] = repo.split("/");
    return `notifications.github.${owner}.${name}.>`;
  });

  const runFatal = async (error: unknown): Promise<void> => {
    if (deps.fatal) {
      await deps.fatal(error);
      return;
    }
    process.exit(1);
  };

  /**
   * Parses and classifies one durable delivery, resolving it through
   * exactly one of `control`'s ack/nak/term, or exiting the process:
   * - JSON or envelope schema failing to parse is poison — termed after a
   *   loud log naming the subject and stream/consumer sequence.
   * - A reducer throw (`DurableReducerFailure`) is poison too — termed,
   *   then fatal (the reducer may have partially mutated live state
   *   before throwing; see `applyDurableEvent`'s doc comment).
   * - Any other failure from a reducer-derived event (`DurableFatalFailure`
   *   — a non-404 effect dispatch or a rejected `saveState`) is fatal:
   *   logged once, then `deps.fatal`, with no ack or nak — the broker
   *   still holds the message, and the restarted process's redelivery
   *   runs against state reloaded from the last successful save.
   * - Anything else (a rejected GitHub-mention publish, including a 404)
   *   naks with a fixed delay: a mention has no reducer or saved state,
   *   so a normal retry is correct and nothing is fatal about it.
   * - Otherwise (`handleEnvelope` resolved): every effect already
   *   dispatched and state is already saved, so just ack.
   */
  const processDurableMessage = async (
    subject: string,
    data: string,
    control: DurableMessageControl
  ): Promise<void> => {
    let rawParsed: unknown;
    try {
      rawParsed = JSON.parse(data);
    } catch (error) {
      poisonMessage(subject, control, error instanceof Error ? error.message : String(error));
      return;
    }
    const parsed = EnvelopeSchema.safeParse(rawParsed);
    if (!parsed.success) {
      const eventId =
        typeof rawParsed === "object" &&
        rawParsed !== null &&
        typeof (rawParsed as Record<string, unknown>).event_id === "string"
          ? ((rawParsed as Record<string, unknown>).event_id as string)
          : undefined;
      poisonMessage(subject, control, parsed.error.message, eventId);
      return;
    }
    const envelope = parsed.data as EnvelopeJson;

    try {
      await handleEnvelope(subject, envelope);
    } catch (error) {
      if (error instanceof DurableReducerFailure) {
        try {
          poisonMessage(subject, control, error.message, envelope.event_id);
          // Term only writes a frame to the client's outgoing buffer; without
          // a flush, the process below can exit before it reaches the
          // server, and JetStream would redeliver a "poison" message the
          // daemon already decided to never retry.
          await deps.nats.flush();
        } catch (termOrFlushError) {
          console.error(
            `[legion] failed to term/flush poison message on ${subject}: ${termOrFlushError instanceof Error ? termOrFlushError.message : termOrFlushError}`
          );
        }
        await runFatal(error);
        return;
      }
      if (error instanceof DurableFatalFailure) {
        console.error(
          `[legion] durable message fatally failed on ${subject} event_id=${envelope.event_id} (stream_seq=${control.streamSequence} delivery_seq=${control.deliverySequence}): ${error.message}`
        );
        await runFatal(error);
        return;
      }
      if (!isNoHolderError(error)) {
        console.error(
          `[legion] durable message processing failed on ${subject} event_id=${envelope.event_id} (stream_seq=${control.streamSequence} delivery_seq=${control.deliverySequence}): ${error instanceof Error ? error.message : String(error)}`
        );
      }
      control.nak(DURABLE_NAK_DELAY_MS);
      throw error;
    }

    control.ack();
  };

  // Durable GitHub messages and resync (see `runExclusive`, used by
  // index.ts's resync scheduler) run one at a time, in delivery order,
  // through a single promise chain: both mutate the same `PrState` CI
  // fields (durable checks via writeCiFence/settleCiVerdict, resync via
  // reconcilePrs's GitHub read), so interleaving them could publish an
  // older resync-derived verdict after a newer durable settlement, or vice
  // versa. The core-NATS lanes below (mention/exception) stay concurrent
  // with this queue and with each other: they only ever touch role claims
  // and tree status/locators (via `onException`), never a `PrState`, and
  // no writer ever replaces or restores another writer's in-flight object,
  // so that remaining overlap is safe without serialization.
  let githubQueue: Promise<void> = Promise.resolve();

  const runExclusive = <T>(fn: () => Promise<T>): Promise<T> => {
    const result = githubQueue.then(fn);
    githubQueue = result.then(
      () => {},
      () => {}
    );
    track(result.then(() => {}));
    return result;
  };

  const unsubscribers = [
    deps.nats.consumeDurable(
      GITHUB_STREAM,
      githubDurable,
      githubFilterSubjects,
      (subject, data, control) => {
        runExclusive(() => processDurableMessage(subject, data, control));
      }
    ),
    deps.nats.subscribe("notifications.slack.*.*.mention", (subject, data) => {
      track(handleMessage(subject, data));
    }),
    deps.nats.subscribe("notifications.envoy.exceptions.notifications.role.>", (subject, data) => {
      track(handleMessage(subject, data));
    }),
  ];

  return {
    applyEffects: applyEffectsAndSave,
    runExclusive,
    async publishControllerEvent(payload: { type: string }, envelope: EnvelopeJson): Promise<void> {
      await publishControllerDirect(JSON.stringify(payload), envelope);
    },
    drainControllerNotices(): Promise<void> {
      const result = controllerNoticeQueue.then(() => drainControllerNoticesLocked());
      controllerNoticeQueue = result.then(
        () => {},
        () => {}
      );
      return result;
    },
    async drain(): Promise<void> {
      // Awaited before the daemon's own final state save (see index.ts's `stop()` sequence): a
      // controller-notice publish already in flight when shutdown began, if it succeeds, must
      // have its own splice+`saveState` settled before that final save runs -- otherwise a
      // notice already delivered could still be redelivered on the next restart, since the
      // final save would not yet reflect its removal. `controllerNoticeQueue` never rejects
      // (see its own doc comment), so awaiting it directly is always safe.
      await controllerNoticeQueue;
      while (pending.size > 0) await Promise.allSettled([...pending]);
      if (failures.length > 0) throw new AggregateError(failures, "Event pump processing failed");
    },
    stop(): void {
      controllerDrainDisposed = true;
      controllerDrainSleep.cancel();
      for (const unsubscribe of unsubscribers) unsubscribe();
    },
  };
}

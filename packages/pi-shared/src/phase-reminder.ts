import { randomUUID } from "node:crypto";
import { ROLE_TOPIC_PREFIX, roleForPhase, roleToken } from "@legion/contracts";
import type { LegionIssue, LegionState } from "@legion/contracts/legion-api";
import { messageFor } from "@legion/envoy-client/errors";
import { EnvoyApiError, type PublishInput } from "@legion/envoy-client/transport";
import type { ClaimCapability } from "./legion-phase-ops";
import {
  LAST_MESSAGE_LIMIT,
  type PhaseStallEntry,
  type PhaseStallInput,
  type PhaseStallState,
  reminderText,
  stepPhaseStall,
} from "./phase-stall";
import type { StallLimits } from "./stall-limits";

/**
 * The endless phase reminder (INFRA-220): it owns the phase-stall machine (`src/phase-stall.ts`)
 * for one session, decides how each reminder reaches the worker, and tells the architect when a
 * run of reminders changes nothing. It imports nothing of Oh My Pi — every collaborator is a
 * field, so a unit test drives it without a host, and `extensions/legion.ts` is the only place
 * that knows what `pi` and the daemon client are.
 *
 * The first reminder of a run is the `session_stop` continuation, which the host turns into one
 * more turn of the same session, and it is the only one of that run: every one after it is a
 * timer, so the run settles and the daemon sees the turn end, can deliver a queued task and can
 * land a held suspension. A host that cannot continue at all says so on `settle`, and its first
 * reminder is a timer too. The host's timer cannot be cancelled (`SessionContext.setTimeout`
 * returns void and there is no clearTimeout), so a monotonic generation, bumped by everything
 * that makes an armed timer stale, is the whole defence against two timers sending two reminders.
 */

/** Publishes one Envoy message (`EnvoyClient.publish`). */
export type EnvoyPublish = (input: PublishInput) => Promise<unknown>;

export interface PhaseReminderLog {
  readonly debug: (message: string, context?: Record<string, unknown>) => void;
  readonly warn: (message: string, context?: Record<string, unknown>) => void;
}

export interface PhaseReminderDeps {
  /** Puts one reminder in front of the worker and starts a turn on it (`pi.sendMessage` with the
   * `legion-phase-reminder` custom type, steered, triggering a turn). A host whose delivery can
   * fail — the Claude Code host writes the reminder over the bridge's inbox — returns a promise
   * that rejects when the reminder did not reach the worker: no turn starts on it, so no settle
   * will arm the next one, and the runner uncounts it and arms the next itself. A host that cannot
   * fail that way (Oh My Pi's `pi.sendMessage` is synchronous) returns nothing. */
  readonly sendReminder: (reminder: string) => void | Promise<void>;
  /** The host's managed one-shot timer (`SessionContext.setTimeout`): it cannot be cancelled, so
   * every callback guards itself on the generation it was armed at. */
  readonly armTimer: (callback: () => void | Promise<void>, delayMs: number) => void;
  /** Persists the record where this host's relaunch reads it again — the Oh My Pi transcript
   * entry, the Claude Code state file — on every change of it. */
  readonly saveEntry: (entry: PhaseStallEntry) => void;
  /** This session's own claim, or undefined before it has registered one. */
  readonly claim: () => ClaimCapability | undefined;
  /** The daemon's whole state, read as the `legion` tool's `read_record` reads it. `signal` is
   * aborted once the record check's bound passes without an answer, and the read is to abort with
   * it (`LegionDaemonClient.state`'s `signal`), so a daemon that hangs is left no open request. */
  readonly readState: (signal: AbortSignal) => Promise<LegionState>;
  /** The Envoy listener the architect notice goes to, or undefined when Envoy is off for this
   * session. Built lazily and once by the caller. */
  readonly publisher: () => EnvoyPublish | undefined;
  /** `LEGION_PROJECT`: the project token every role topic carries. */
  readonly project: string | undefined;
  readonly limits: StallLimits;
  readonly log: PhaseReminderLog;
  /** The bound on one daemon read; `RECORD_READ_BOUND_MS` when it is left out. */
  readonly recordBoundMs?: number;
}

export interface PhaseReminder {
  /** `session_start`: the record this host persisted, or undefined when it holds none yet. */
  readonly restore: (entry: PhaseStallEntry | undefined) => void;
  /** `session_start`, once the claim bootstrap has returned: the resume reminder, for a worker
   * the daemon relaunched into an open phase that nothing else will wake. */
  readonly armResumeReminder: () => void;
  /** `message_start`: a turn is running, and every armed timer is now stale. */
  readonly turnStarted: () => void;
  /** An input the session's hooks classified; `settle` and `reminded` are this module's own. */
  readonly apply: (input: Exclude<PhaseStallInput, { kind: "settle" | "reminded" }>) => void;
  /** `session_stop`: the continuation text to return to the host, or undefined. A host that
   * cannot turn this settle into one more turn of the same session — Claude Code ignores what a
   * `StopFailure` hook returns — says so with `canContinue: false`, and the reminder it earned
   * comes from the timer instead, under every guard an armed reminder has. */
  readonly settle: (
    lastAssistantText: string,
    options?: { readonly canContinue?: boolean }
  ) => Promise<string | undefined>;
  /** `session_switch`, `session_branch`, `session_tree`. */
  readonly sessionChanged: () => void;
}

/** How long the record check waits for the daemon. `createLegionDaemonClient` puts no deadline on
 * its fetch, and `session_stop` is a hook the host awaits, so the check bounds the read itself and
 * aborts it at the bound. */
export const RECORD_READ_BOUND_MS = 5_000;

/** The listener's "no live session holds that role": a 404 whose body names `unclaimed` or
 * `holder_lapsed` (`packages/envoy/cmd/listener/api.go`). The next delivered reminder tries the
 * same notice again, under the same idempotency key. */
function roleUnheld(error: unknown): boolean {
  if (!(error instanceof EnvoyApiError) || error.details.status !== 404) return false;
  let body: unknown;
  try {
    body = JSON.parse(error.details.responseBody);
  } catch {
    return false;
  }
  if (typeof body !== "object" || body === null || !("reason" in body)) return false;
  return body.reason === "unclaimed" || body.reason === "holder_lapsed";
}

/** `work`'s answer, or a rejection once `ms` have passed without one, when the signal `work` was
 * handed is aborted so what it started does not outlive the bound (`withinBudget` in
 * `src/capability-report.ts` is the same shape, with a fallback answer in place of the throw). */
async function withBound<T>(work: (signal: AbortSignal) => Promise<T>, ms: number): Promise<T> {
  const controller = new AbortController();
  const deadline = Promise.withResolvers<T>();
  const timer = setTimeout(() => {
    const late = new Error(`no answer within ${ms}ms`);
    deadline.reject(late);
    controller.abort(late);
  }, ms);
  try {
    return await Promise.race([work(controller.signal), deadline.promise]);
  } finally {
    clearTimeout(timer);
  }
}

/** What the record check decided, and the record it read, for the architect notice that may
 * follow it: the same reminder never reads the daemon twice. */
interface RecordCheck {
  readonly reminds: boolean;
  readonly record?: LegionIssue;
}

export function createPhaseReminder(deps: PhaseReminderDeps): PhaseReminder {
  const boundMs = deps.recordBoundMs ?? RECORD_READ_BOUND_MS;
  let stall: PhaseStallState = { state: "closed", count: 0 };
  let reported = false;
  let reportKey: string | undefined;
  /** Bumped by everything that makes an armed timer stale. */
  let generation = 0;
  let turnRunning = false;
  /** Whether a continuation has already been delivered in the run that is now settling. The count
   * cannot say so: the continuation's own turn is one the worker is being told to call a tool in,
   * so a `tool-call` or an `inbound-event` puts the count back to 0 mid-run. At most one
   * continuation per run: the run then settles, the daemon sees the turn end, and the next
   * reminder comes from the timer with its delay. */
  let continued = false;
  /** The head of what the worker said as its last turn settled: the architect notice quotes it,
   * and it rides the record, so a notice the resume reminder of a relaunched worker earns still
   * names what the worker said before the relaunch. */
  let lastMessage = "";
  /** The entry last saved or restored, so only a change of the recorded value is saved. */
  let recorded = "";

  const entryOf = (): PhaseStallEntry => ({
    state: stall.state,
    count: stall.count,
    reported,
    ...(reportKey === undefined ? {} : { reportKey }),
    ...(lastMessage === "" ? {} : { lastMessage }),
  });

  const writeEntry = (): void => {
    const entry = entryOf();
    const written = JSON.stringify(entry);
    if (written === recorded) return;
    recorded = written;
    deps.saveEntry(entry);
  };
  recorded = JSON.stringify(entryOf());

  const advance = (input: PhaseStallInput): string | undefined => {
    const step = stepPhaseStall(stall, input);
    stall = step.state;
    // A count back at zero is a worker that has done something: the next run of reminders reports
    // afresh, under a key of its own.
    if (stall.count === 0) {
      reported = false;
      reportKey = undefined;
    }
    writeEntry();
    return step.reminder;
  };

  /**
   * D5: this session's own issue record, read as `read_record` reads it, bounded. The reminder is
   * dropped only when the record says this role no longer works this phase — the shape of a worker
   * that completed its phase from the shell instead of the tool. Everything else fails open and is
   * logged at debug: an unreachable daemon is exactly when a stalled worker must still be nudged.
   */
  const recordCheck = async (): Promise<RecordCheck> => {
    const claim = deps.claim();
    if (claim === undefined) return { reminds: true };
    let record: LegionIssue | undefined;
    try {
      record = (await withBound(deps.readState, boundMs)).issues[claim.issue];
    } catch (error) {
      deps.log.debug("legion: the daemon did not answer the phase reminder's record check", {
        issue: claim.issue,
        error: messageFor(error),
      });
      return { reminds: true };
    }
    if (record === undefined) {
      deps.log.debug("legion: the daemon holds no record for this phase reminder's issue", {
        issue: claim.issue,
      });
      return { reminds: true };
    }
    if (roleForPhase(record.phase) !== claim.role) {
      deps.log.debug("legion: this role no longer works this phase; no reminder is sent", {
        issue: claim.issue,
        phase: record.phase,
        role: claim.role,
      });
      return { reminds: false, record };
    }
    return { reminds: true, record };
  };

  /**
   * D6: one Envoy message to the issue's architect, when a delivered reminder brought the count to
   * the threshold. `reportKey` is minted at the first attempt of a run of reminders and kept until
   * the count resets, so a publish that timed out after the listener accepted it is not delivered
   * twice, and a failure leaves `reported` false for the next reminder to try again. The key also
   * names the run an acceptance belongs to: one that lands after the run ended acknowledges
   * nothing.
   */
  const tellArchitect = async (record: LegionIssue | undefined): Promise<void> => {
    const claim = deps.claim();
    if (claim === undefined) return;
    const publish = deps.publisher();
    if (publish === undefined) {
      deps.log.debug("legion: Envoy is off for this session; the architect is not told", {
        issue: claim.issue,
        reminders: stall.count,
      });
      return;
    }
    reportKey ??= randomUUID();
    const key = reportKey;
    writeEntry();
    const phase = record?.phase ?? "unknown";
    const reminders = stall.count;
    try {
      // An issue whose own architect claim is live owns this worker; otherwise the tree's does.
      const live =
        record?.architect !== undefined &&
        record.architect.state !== "retired" &&
        record.architect.state !== "failed";
      const topic =
        ROLE_TOPIC_PREFIX +
        roleToken(deps.project ?? "", live ? claim.issue : claim.tree, "architect");
      await publish({
        topic,
        message:
          `The ${claim.role} of ${claim.issue} has let ${reminders} phase reminders go unanswered: ` +
          `its turns keep ending with the phase ${phase} still open and no successful ` +
          `handoff_complete, and nothing it does clears the reminder. ` +
          `What it last said was: ${lastMessage}`,
        // The listener refuses an object here: the facts travel as a JSON string.
        payload: JSON.stringify({
          issue: claim.issue,
          role: claim.role,
          phase,
          reminders,
          lastMessage,
        }),
        idempotencyKey: key,
        sourceSessionID: claim.sessionID,
      });
      // The publish has no deadline of its own, so the worker's own turn may have reset the count
      // while it was in flight, and a later run may have begun. That run has a key of its own, or
      // none yet: this acceptance is not its notice.
      if (reportKey !== key) return;
      reported = true;
      writeEntry();
    } catch (error) {
      if (roleUnheld(error)) {
        deps.log.debug("legion: no live session holds the architect role; telling it again later", {
          issue: claim.issue,
          reminders,
        });
        return;
      }
      // Any other failure — a 5xx, a 400 or 403, a timeout — leaves `reported` false too, and the
      // next delivered reminder of this run publishes again under the same key. This departs from
      // the spec's "logged and dropped" on purpose: the one key makes the retry safe against a
      // publish the listener did accept, and a listener that is failing is exactly when the
      // architect most needs telling.
      deps.log.warn("legion: the architect was not told its worker is stalled", {
        issue: claim.issue,
        reminders,
        error: messageFor(error),
      });
    }
  };

  const deliver = async (reminder: string, record: LegionIssue | undefined): Promise<void> => {
    // Counted before it is sent, so a reminder the worker was given is never uncounted: a host
    // whose turn starts inside `sendReminder` saves the count before the turn can move it.
    const uncounted = stall;
    advance({ kind: "reminded" });
    const counted = stall;
    generation += 1;
    const sentAt = generation;
    try {
      await deps.sendReminder(reminder);
    } catch (error) {
      deps.log.warn("legion: a phase reminder did not reach this worker; it comes again later", {
        error: messageFor(error),
      });
      // It reached no one, so it counts for nothing, unless the state has moved on since. No turn
      // starts on it, so no settle arms the next one: this does, under the usual guard, unless
      // something has happened since — a turn that started arms its own when it settles.
      if (stall === counted) {
        stall = uncounted;
        writeEntry();
      }
      if (generation === sentAt) armReminder(reminder, deps.limits.reminderMs);
      return;
    }
    if (reported || stall.count < deps.limits.architectAfter) return;
    await tellArchitect(record);
  };

  /**
   * D4: the one guard. A timer fires only while nothing has happened since it was armed, the phase
   * is still open, no turn is running, and the record check passes. The check awaits the daemon, so
   * the generation and the turn are read again after it.
   */
  const armReminder = (reminder: string, delayMs: number): void => {
    const armedAt = generation;
    deps.armTimer(async () => {
      if (generation !== armedAt || stall.state === "closed" || turnRunning) return;
      const check = await recordCheck();
      if (!check.reminds) return;
      if (generation !== armedAt || turnRunning) return;
      await deliver(reminder, check.record);
    }, delayMs);
  };

  return {
    restore: (entry) => {
      stall =
        entry === undefined
          ? { state: "closed", count: 0 }
          : { state: entry.state, count: entry.count };
      // The run of reminders the restored count belongs to keeps its notice: a relaunch mid-run
      // neither repeats one the listener accepted nor mints a second key for one it refused.
      reported = entry?.reported ?? false;
      reportKey = entry?.reportKey;
      lastMessage = entry?.lastMessage ?? "";
      recorded = JSON.stringify(entryOf());
    },
    armResumeReminder: () => {
      if (stall.state === "closed") return;
      armReminder(reminderText(""), deps.limits.resumeMs);
    },
    turnStarted: () => {
      generation += 1;
      turnRunning = true;
    },
    apply: (input) => {
      if (input.kind === "phase-closed") generation += 1;
      advance(input);
    },
    settle: async (text, options) => {
      turnRunning = false;
      generation += 1;
      // Only a phase that is not closed can earn the notice that quotes these words, so a closed
      // one records none: a finished worker's chatter writes nothing.
      if (stall.state !== "closed") lastMessage = text.slice(0, LAST_MESSAGE_LIMIT);
      const before = stall.count;
      const reminder = advance({ kind: "settle", lastAssistantText: text });
      // Every outcome but the continuation ends the run here, so the next one may continue again.
      if (stall.state === "waiting") {
        // The machine sends no reminder into a quiet wait, so the timer carries the one this
        // settling message earned, for when the wait has been long enough.
        continued = false;
        armReminder(reminderText(text), deps.limits.waitingMs);
        return undefined;
      }
      // A closed phase earns none.
      if (reminder === undefined) {
        continued = false;
        return undefined;
      }
      // The continuation is the first reminder of a run and the only one of it: the worker has
      // been reminded already (`before`), has had this run's continuation (`continued`), or is on
      // a host that cannot continue at all, and the reminder comes from a timer instead. The run
      // then settles, the daemon sees the turn end, and the next reminder comes with its delay.
      if (before !== 0 || continued || options?.canContinue === false) {
        continued = false;
        armReminder(reminder, deps.limits.reminderMs);
        return undefined;
      }
      if (!(await recordCheck()).reminds) {
        continued = false;
        return undefined;
      }
      advance({ kind: "reminded" });
      generation += 1;
      continued = true;
      return reminder;
    },
    sessionChanged: () => {
      generation += 1;
    },
  };
}

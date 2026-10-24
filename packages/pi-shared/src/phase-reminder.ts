import { randomUUID } from "node:crypto";
import { ROLE_TOPIC_PREFIX, roleForPhase, roleToken } from "@legion/contracts";
import type { LegionIssue, LegionState } from "@legion/contracts/legion-api";
import { messageFor } from "@legion/envoy-client/errors";
import { EnvoyApiError, type PublishInput } from "@legion/envoy-client/transport";
import type { ClaimCapability } from "./legion-phase-ops";
import {
  type PhaseStall,
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
 * more turn of the same session. Every one after it is a timer: the run must settle so the daemon
 * sees the turn end, can deliver a queued task and can land a held suspension. The host's timer
 * cannot be cancelled (`SessionContext.setTimeout` returns void and there is no clearTimeout), so
 * a monotonic generation, bumped by everything that makes an armed timer stale, is the whole
 * defence against two timers sending two reminders.
 */

/** Publishes one Envoy message (`EnvoyClient.publish`). */
export type EnvoyPublish = (input: PublishInput) => Promise<unknown>;

export interface PhaseReminderLog {
  readonly debug: (message: string, context?: Record<string, unknown>) => void;
  readonly warn: (message: string, context?: Record<string, unknown>) => void;
}

/** What one recorded entry carries. The machine never reads `reported` and `reportKey`, and each
 * host persists them its own way — the Oh My Pi transcript entry, the Claude Code state file —
 * but they ride the same record, so a relaunch mid-run neither repeats an accepted notice nor
 * loses the key a refused one is retried under. */
export interface PhaseStallEntry {
  readonly state: PhaseStall;
  readonly count: number;
  readonly reported: boolean;
  readonly reportKey?: string;
}

export interface PhaseReminderDeps {
  /** Puts one reminder in front of the worker and starts a turn on it (`pi.sendMessage` with the
   * `legion-phase-reminder` custom type, steered, triggering a turn). */
  readonly sendReminder: (reminder: string) => void;
  /** The host's managed one-shot timer (`SessionContext.setTimeout`): it cannot be cancelled, so
   * every callback guards itself on the generation it was armed at. */
  readonly armTimer: (callback: () => void | Promise<void>, delayMs: number) => void;
  /** Persists the record where this host's relaunch reads it again — the Oh My Pi transcript
   * entry, the Claude Code state file — on every change of it. */
  readonly saveEntry: (entry: PhaseStallEntry) => void;
  /** This session's own claim, or undefined before it has registered one. */
  readonly claim: () => ClaimCapability | undefined;
  /** The daemon's whole state, read as the `legion` tool's `read_record` reads it. */
  readonly readState: () => Promise<LegionState>;
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
  /** `session_stop`: the continuation text to return to the host, or undefined. */
  readonly settle: (lastAssistantText: string) => Promise<string | undefined>;
  /** `session_switch`, `session_branch`, `session_tree`. */
  readonly sessionChanged: () => void;
}

/** How long the record check waits for the daemon. `createLegionDaemonClient` puts no deadline on
 * its fetch, and `session_stop` is a hook the host awaits, so the check bounds the read itself. */
export const RECORD_READ_BOUND_MS = 5_000;

/** The head of the worker's last message the architect notice carries. */
const LAST_MESSAGE_LIMIT = 500;

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

/** `work`'s answer, or a rejection once `ms` have passed without one (`withinBudget` in
 * `src/capability-report.ts` is the same shape, with a fallback answer in place of the throw). */
async function withBound<T>(work: Promise<T>, ms: number): Promise<T> {
  const deadline = Promise.withResolvers<T>();
  const timer = setTimeout(() => deadline.reject(new Error(`no answer within ${ms}ms`)), ms);
  try {
    return await Promise.race([work, deadline.promise]);
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
  /** What the worker said as its last turn settled: the architect notice quotes its head. */
  let lastAssistantText = "";
  /** The entry last saved or restored, so only a change of the recorded value is saved. */
  let recorded = "";

  const entryOf = (): PhaseStallEntry => ({
    state: stall.state,
    count: stall.count,
    reported,
    ...(reportKey === undefined ? {} : { reportKey }),
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
      record = (await withBound(deps.readState(), boundMs)).issues[claim.issue];
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
   * twice, and a refusal leaves `reported` false for the next reminder to try again.
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
    writeEntry();
    const phase = record?.phase ?? "unknown";
    const head = lastAssistantText.slice(0, LAST_MESSAGE_LIMIT);
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
          `handoff_complete, and nothing it does clears the reminder. What it last said was: ${head}`,
        // The listener refuses an object here: the facts travel as a JSON string.
        payload: JSON.stringify({
          issue: claim.issue,
          role: claim.role,
          phase,
          reminders,
          lastMessage: head,
        }),
        idempotencyKey: reportKey,
        sourceSessionID: claim.sessionID,
      });
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
      deps.log.warn("legion: the architect was not told its worker is stalled", {
        issue: claim.issue,
        reminders,
        error: messageFor(error),
      });
    }
  };

  const deliver = async (reminder: string, record: LegionIssue | undefined): Promise<void> => {
    advance({ kind: "reminded" });
    generation += 1;
    deps.sendReminder(reminder);
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
    settle: async (text) => {
      turnRunning = false;
      generation += 1;
      lastAssistantText = text;
      const before = stall.count;
      const reminder = advance({ kind: "settle", lastAssistantText: text });
      if (stall.state === "waiting") {
        // The machine sends no reminder into a quiet wait, so the timer carries the one this
        // settling message earned, for when the wait has been long enough.
        armReminder(reminderText(text), deps.limits.waitingMs);
        return undefined;
      }
      // A closed phase earns none.
      if (reminder === undefined) return undefined;
      if (before !== 0) {
        // The worker has already been reminded and done nothing about it: let the run settle, and
        // deliver this reminder from a timer instead.
        armReminder(reminder, deps.limits.reminderMs);
        return undefined;
      }
      if (!(await recordCheck()).reminds) return undefined;
      advance({ kind: "reminded" });
      generation += 1;
      return reminder;
    },
    sessionChanged: () => {
      generation += 1;
    },
  };
}

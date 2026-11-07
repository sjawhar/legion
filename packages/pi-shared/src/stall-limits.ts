import type { LegionRole } from "@legion/contracts";

/**
 * The numbers the endless phase reminder (`src/phase-reminder.ts`) and the worker's own watch on
 * its open tool calls run on, read from the worker's own environment. The daemon passes each
 * `LEGION_STALL_*` its own environment holds on to the worker, and the defaults below,
 * production's, apply to every one it does not hold: a variable is there only so a deployment, or
 * a test, can move one.
 *
 * `callBoundMs` of 0 switches the call watch off for that worker: no call is judged overdue by
 * the bound, so nothing is told and no step of the recovery ladder runs. A reader of these
 * numbers honours that rather than reading 0 as a call that is overdue the moment it starts.
 */

export interface StallLimits {
  /** How long after a settle that earned a reminder the plugin waits before delivering it, once
   * the first reminder of the run has gone out as the `session_stop` continuation. */
  readonly reminderMs: number;
  /** How long a WAITING reply buys before the reminder comes again. */
  readonly waitingMs: number;
  /** How long after a resume on an open phase the plugin waits before reminding. */
  readonly resumeMs: number;
  /** How many delivered reminders in a row change nothing before the architect is told. */
  readonly architectAfter: number;
  /** The grace over a call's own declared deadline: a call judged by a deadline is overdue only
   * once it has been open past that deadline plus this. */
  readonly callGraceMs: number;
  /** The bound for a call no deadline judges — an `eval` call, whose deadline the host may pause;
   * a call that declares 0, for which the host arms none; a tool that takes no deadline, a
   * subagent among them. 0 switches the call watch off for this worker. Its default is the
   * role's (`defaultCallBoundMs`); the variable, where the deployment sets one, is one value for
   * every role, because the daemon copies one name from its own environment to every session. */
  readonly callBoundMs: number;
  /** The gap between the recovery ladder's steps: each step runs only because the step before it
   * did not end the call within this. */
  readonly stepMs: number;
}

/** The log one bad value writes its `warn` to (`logger` from `@oh-my-pi/pi-utils` in production). */
export interface StallLimitsLog {
  readonly warn: (message: string, context?: Record<string, unknown>) => void;
}

export const DEFAULT_STALL_LIMITS: StallLimits = {
  // 5 minutes is one model turn per stalled worker, and it leaves the 60 s idle window a Stage 3
  // e2e measures to the single `session_stop` continuation reminder.
  reminderMs: 300_000,
  // 15 minutes is longer than a CI run here, so a worker that said what it waits on is not woken
  // in the middle of the wait.
  waitingMs: 900_000,
  // 2 minutes is longer than the daemon's re-send of an interrupted task after `ready`, so a
  // resumed worker the daemon is about to prompt again is prompted before it is reminded.
  resumeMs: 120_000,
  // 3 unanswered reminders is about ten minutes of a worker doing nothing at all.
  architectAfter: 3,
  // 5 minutes over a call's own declared deadline is longer than the host's own wait for a
  // native cancellation, so a call the host is already ending is never one this watch judges.
  callGraceMs: 300_000,
  // 1 hour is the bound of every role but the implementer and the reviewer, which get four
  // (`defaultCallBoundMs`). Measured over the 24 hours to 2026-10-25, across 2,059 completed
  // turns: the longest silence inside an open turn was 2 h 24 min for an implementer running a
  // subagent, 1 hour for a reviewer and under 22 minutes for every other role.
  callBoundMs: 3_600_000,
  // 2 minutes between the ladder's steps. An ordinary foreground shell call takes 60.0 seconds to
  // close its own record on this binary (measured four times, dispatch://INFRA-1274's plan), so a
  // shorter gap would run the next step while the step before it was still landing; and two gaps
  // add four minutes to the ladder the daemon's own backstop bound must stay above.
  stepMs: 120_000,
};

/** 4 hours: the bound of the two roles that legitimately wait that long inside one call. */
const LONG_CALL_BOUND_MS = 14_400_000;

/** The roles whose unjudged call is given `LONG_CALL_BOUND_MS` rather than the hour every other
 * role and the controller get: an implementer waits on a subagent of its own, and a reviewer on
 * the pair of deep reviews it dispatches. Measured over the 24 hours to 2026-10-25, across 2,059
 * completed turns, the longest silence inside an open turn was 2 h 24 min for an implementer and
 * 1 hour for a reviewer, against under 22 minutes for every other role. */
const LONG_CALL_BOUND_ROLES: readonly string[] = [
  "implementer",
  "reviewer",
] satisfies readonly LegionRole[];

/** The default `callBoundMs` for `role`, the word this worker's `LEGION_ROLE` holds: 4 hours for
 * an implementer and a reviewer, 1 hour for every other role, for the controller (whose role word
 * is `controller`, no `LegionRole`) and for a session that names none. */
export function defaultCallBoundMs(role: string | undefined): number {
  return role !== undefined && LONG_CALL_BOUND_ROLES.includes(role)
    ? LONG_CALL_BOUND_MS
    : DEFAULT_STALL_LIMITS.callBoundMs;
}

/** One variable's value: the default when it is unset, and the default with one `warn` naming the
 * variable when it is not an integer of `least` or more. */
function integerSetting(
  env: Record<string, string | undefined>,
  log: StallLimitsLog,
  name: string,
  fallback: number,
  least: number
): number {
  const raw = env[name];
  if (raw === undefined) return fallback;
  const value = raw.trim() === "" ? Number.NaN : Number(raw);
  if (!Number.isInteger(value) || value < least) {
    log.warn(`legion: ${name} is not an integer of ${least} or more; using ${fallback}`, {
      value: raw,
    });
    return fallback;
  }
  return value;
}

/**
 * The limits this worker runs on. No variable is required. `LEGION_STALL_ARCHITECT_AFTER` is
 * clamped to a minimum of 2, so the threshold can never be reached by the `session_stop`
 * continuation reminder alone, which is the one reminder a worker always gets. `role` is this
 * session's own role word (`LEGION_ROLE`), which decides the bound's default alone: the variable,
 * where the deployment sets one, overrides it for every role alike.
 */
export function stallLimits(
  env: Record<string, string | undefined>,
  log: StallLimitsLog,
  role: string | undefined
): StallLimits {
  return {
    reminderMs: integerSetting(
      env,
      log,
      "LEGION_STALL_REMINDER_MS",
      DEFAULT_STALL_LIMITS.reminderMs,
      0
    ),
    waitingMs: integerSetting(
      env,
      log,
      "LEGION_STALL_WAITING_MS",
      DEFAULT_STALL_LIMITS.waitingMs,
      0
    ),
    resumeMs: integerSetting(env, log, "LEGION_STALL_RESUME_MS", DEFAULT_STALL_LIMITS.resumeMs, 0),
    architectAfter: integerSetting(
      env,
      log,
      "LEGION_STALL_ARCHITECT_AFTER",
      DEFAULT_STALL_LIMITS.architectAfter,
      2
    ),
    callGraceMs: integerSetting(
      env,
      log,
      "LEGION_STALL_CALL_GRACE_MS",
      DEFAULT_STALL_LIMITS.callGraceMs,
      0
    ),
    callBoundMs: integerSetting(
      env,
      log,
      "LEGION_STALL_CALL_BOUND_MS",
      defaultCallBoundMs(role),
      0
    ),
    stepMs: integerSetting(env, log, "LEGION_STALL_STEP_MS", DEFAULT_STALL_LIMITS.stepMs, 0),
  };
}

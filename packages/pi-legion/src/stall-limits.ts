/**
 * The four numbers the endless phase reminder runs on (`src/phase-reminder.ts`), read from the
 * worker's own environment. Nothing in the daemon sets them: the defaults below are production's,
 * and a variable is there only so a deployment, or a test, can move one.
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
};

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
 * continuation reminder alone, which is the one reminder a worker always gets.
 */
export function stallLimits(
  env: Record<string, string | undefined>,
  log: StallLimitsLog
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
  };
}

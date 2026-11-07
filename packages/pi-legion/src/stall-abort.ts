import type { PiApi, SessionContext } from "@legion/pi-shared/pi-types";
import type { StallWatchdogAborter } from "@legion/pi-shared/stall-watchdog";

/**
 * The second step of the stall ladder (INFRA-1274, R4): the turn, ended, and the worker's work
 * started again. `packages/pi-shared/src/stall-watchdog.ts` decides when the step runs and writes
 * the message's words (`restartMessage`); this module is the one place that reaches the host for
 * either half of it.
 *
 * **The abort.** `ctx.abort()` ends the turn that runs now, keeps the process and the session,
 * and ends the running call's own child processes. Measured on `omp/18.16.0` in `--mode rpc` on
 * 2026-11-07, from an extension that aborted a never-returning `bash` call: it returns a Promise,
 * the call's result comes back in the same millisecond as `Command aborted` with `isError: true`,
 * the turn ends at once, the command's own children are gone 30 s later while the agent is still
 * alive, and the same process answered an RPC `get_state` and ran a fresh turn 34 s after the
 * abort. So this step costs one turn and nothing else, which is why it runs before the step that
 * costs the process.
 *
 * **`abort` is asked for by `typeof`.** It is present on the handler context but is not an own
 * enumerable property — `Object.keys(ctx)` lists `ui` alone (measured the same day) — so this
 * module asks what the member is, never whether a key list holds it. It is called on the context
 * it came from, since it is the host's method for that session's run.
 *
 * **The message the worker then sends itself.** The daemon counts a task served by the turn that
 * ends and sends nothing after it, so a turn ended here would otherwise leave the worker idle
 * with its work unfinished. The worker puts one message in front of its own session instead,
 * which the host turns into a turn because the session is idle once the abort has ended the turn:
 * the same `pi.sendMessage` delivery the phase reminder starts a turn of a settled worker with
 * (`extensions/legion.ts:189-193`).
 *
 * **It carries its own custom type** and is never sent as a user message: a `role: "user"`
 * message is what the phase check reads as a new assignment (`inboundKind`,
 * `pi-shared/src/phase-stall.ts:172-176`), so a worker restarting itself would reset that check's
 * own count of what it has reminded about.
 *
 * **Both halves fail loudly, and the ladder goes on.** A host with no `abort()`, an `abort()`
 * that throws or whose Promise rejects, and a message the host refuses each throw from here; the
 * detector records what happened and carries the call to the last step at the next gap, rather
 * than leaving the ladder with a turn it ended and nothing to restart it.
 */

/** The custom type the restart message carries, beside the phase reminder's
 * `legion-phase-reminder`: a person reading the session tells the worker's own restart from an
 * assignment, and the phase check counts it as neither. */
export const STALL_RESTART_MESSAGE = "legion-stall-restart";

export interface StallAborterDeps {
  /** The handler context the plugin kept from `session_start`, which is where `abort` lives;
   * undefined before the host has handed one over, which ends no turn. */
  readonly context: () => SessionContext | undefined;
  /** `pi.sendMessage`: the host's own delivery, which queues the message and starts a turn on
   * it. */
  readonly sendMessage: PiApi["sendMessage"];
}

/** The detector's `StallWatchdogAborter`: the host's `abort()`, and the one message the worker
 * sends its own session to start the work again. */
export function createStallAborter(deps: StallAborterDeps): StallWatchdogAborter {
  return {
    abortTurn: async (): Promise<void> => {
      const context = deps.context();
      if (context === undefined) {
        throw new Error("this session has no handler context, so it has no turn to end");
      }
      const { abort } = context;
      if (typeof abort !== "function") {
        throw new Error("this host has no ctx.abort(), so the turn could not be ended");
      }
      await abort.call(context);
    },

    sendMessage: (text: string): void => {
      deps.sendMessage(
        { customType: STALL_RESTART_MESSAGE, content: text, display: true },
        { deliverAs: "steer", triggerTurn: true }
      );
    },
  };
}

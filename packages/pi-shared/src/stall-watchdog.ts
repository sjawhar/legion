import { messageFor } from "@legion/envoy-client/errors";
import type {
  ToolCallEvent,
  ToolExecutionEndEvent,
  ToolExecutionStartEvent,
  ToolResultEvent,
} from "./pi-types";
import type { StallLimits } from "./stall-limits";

/**
 * The worker's watch on its own tool calls (INFRA-1274), and the ladder it drives when one of them
 * stops. Legion has five stall detectors already, each with a threshold convention of its own —
 * the phase reminder, the daemon's ask of a restored claim, the NATS pull heartbeat, the warm
 * lease's missed-beat budget and the pod update round's notice — and this is the sixth detector
 * with no sixth convention: a caller reaches for this module rather than writing another file, as
 * `packages/daemon/internal/backoff/backoff.go` is already the one statement of the wait before
 * trying again.
 *
 * **The invariant.** A missed beat is one tool call that has outlived the deadline its own tool
 * promised: the call's execution has been running longer than the deadline its arguments declare,
 * or than the tool's own default where they declare none, plus `callGraceMs`. A call that promised
 * nothing — one whose deadline the host may pause, one that declares 0, and a tool that takes no
 * deadline at all — has no promise to outlive, so the only thing bounded for it is a whole turn's
 * silence, and that is its role's `callBoundMs`.
 *
 * **What a missed beat is not.** It is not a long turn: a turn that keeps calling tools is a
 * succession of short calls and none of them is overdue. It is not a slow model: the clock starts
 * at `tool_execution_start`, so neither the queue nor the approval gate nor a provider's thinking
 * time is counted against a call. It is not a call whose deadline the host may pause — `eval`
 * pauses across agent and completion waits, package installation and delegated subagent calls —
 * which is judged by its role's bound alone and never by a deadline. It is not a background job:
 * a call the host hands to its async job manager has closed its own record, and what runs on
 * after it belongs to the session, not to the call.
 *
 * **Pure.** Every collaborator is a field, as `src/phase-reminder.ts` states for the reminder, so
 * a unit test drives the whole ladder with no host, and this module imports no Oh My Pi and no
 * `node:` module: `packages/claude-envoy` bundles `packages/pi-shared` with no `node_modules`
 * (`src/envoy-notice.ts`); its one package import is `@legion/envoy-client/errors`, which imports
 * nothing at all and which `src/phase-reminder.ts` already bundles that way. The `/proc` reader,
 * the process ending, the abort, the owner's notice and the exit counter are implemented in
 * `packages/pi-legion` and passed in.
 *
 * **What was measured, and when.** Each of these cost an hour to establish and decides something
 * here. All were measured on 2026-11-07 against `/opt/omp/bin/omp` (`omp/18.16.0`) in `--mode rpc`
 * with `async.enabled` and `bash.autoBackground.enabled` on, as every Legion role runs
 * (`packages/daemon/internal/runtime/tmux/rolesession_omp_test.go:16-51`).
 *
 * 1. **An ordinary foreground `bash` call closes its own record at 60.0 seconds** — measured four
 *    times on 2026-11-07, at 60.008 s, 60.009 s, with `timeout: 600` at 60.004 s and with
 *    `timeout: 0` at 60.017 s. The host auto-backgrounds it and its `tool_execution_end` arrives
 *    with `isError: false` and `result.details.async.state: "running"`, 540 seconds before the
 *    shell tool's 300-second deadline plus the 5-minute grace could judge it. So the deadline
 *    rule never reaches an ordinary shell call. It reaches a `pty: true` call, which stays in
 *    the foreground and writes nothing at all after its `tool_execution_start` — measured twice,
 *    170 s with a builtin command and 100 s with one that forks: no end, no `turn_end`, no
 *    `agent_end` — and that is this issue's own shape. The shell branch therefore serves the PTY
 *    route and the two foreground fall-backs this deployment has on record
 *    (dispatch://INFRA-763's wedged shell and dispatch://INFRA-515's starved scheduler pool),
 *    and not the ordinary one.
 * 2. **`/proc/self/environ` reads empty under this pod's gVisor** (2026-11-07), while `env` from
 *    inside a process and `/proc/<other pid>/environ` from outside both read correctly
 *    (`FOO=bar sh -c "tr '\0' '\n' < /proc/self/environ | grep -c ."` printed 0 against 151 for
 *    the same read through `/proc/$$/environ`). A `StallWatchdogProcesses` therefore reads other
 *    pids only, and a tree it cannot read is reported as unread rather than as empty.
 * 3. **A call blocked before it ran has no host event of its own.** A block is a `tool_call`
 *    handler's return value and a peer extension's block is invisible here, so there is nothing to
 *    subscribe to. Measured the same day: a `tool_call` handler that throws, and one that
 *    outruns the host's 30 s `extensionHandlers.toolCallTimeoutMs`, each make the host fail the
 *    call closed, and in each case the call still gets its `tool_execution_start` with its
 *    arguments and then an error `tool_execution_end` one millisecond later. So a blocked call
 *    closes through `executionEnded`, like every other call, and nothing leaks.
 * 4. **The worker's own exit cap is `STALL_EXIT_CAP`**, one below the daemon's
 *    `launch_failure_limit`, whose default is 3
 *    (`packages/daemon/internal/config/config.go:253`); a deployment may move that key, and this
 *    plugin cannot read it. Under Kubernetes the count lives in the pod's own `LEGION_STATE_DIR`,
 *    so a pod replacement resets it while the daemon's death budget is not reset.
 */

/** How the host clamps a deadline a call's arguments declare: `0` disables it, and any other
 * number is clamped to 1–3600 seconds, as the shell tool's and `eval`'s own argument descriptions
 * say (`omp://tools/bash.md`, `omp://tools/eval.md`, read 2026-11-07). */
const DECLARED_DEADLINE_LEAST_MS = 1_000;
const DECLARED_DEADLINE_MOST_MS = 3_600_000;

/** What one tool promises about how long a call of it may run. */
export interface StallToolDeadline {
  /** The argument that declares this call's own deadline, in seconds. */
  readonly argument: string;
  /** The deadline the host arms when the arguments declare none. */
  readonly defaultMs: number;
}

/**
 * The tools whose deadline the host does not pause, by name: a call of one of them is judged by
 * the deadline rule. There is exactly one today, the shell tool, whose default is 300 seconds.
 * A tool absent from this table takes no deadline this watch can trust and is judged by the
 * role's bound alone.
 */
export const STALL_DEADLINE_TOOLS: Readonly<Record<string, StallToolDeadline>> = {
  bash: { argument: "timeout", defaultMs: 300_000 },
};

/**
 * The tools whose own deadline the host may pause while the call waits legitimately, named so
 * their absence from `STALL_DEADLINE_TOOLS` reads as the decision it is rather than as an
 * oversight. `eval` declares a 30-second default and pauses it across agent and completion waits,
 * package installation and delegated subagent calls, so no number it declares bounds anything;
 * its calls are judged by the role's bound.
 */
export const STALL_PAUSED_DEADLINE_TOOLS: readonly string[] = ["eval"];

/** The ladder's steps, in the order the sweep runs them, each only because the step before it did
 * not end the call within `stepMs`. */
export type StallStep = "end-processes" | "abort-turn" | "exit-process";

export const STALL_LADDER: readonly StallStep[] = ["end-processes", "abort-turn", "exit-process"];

/** How many times one worker ends its own process before it ends nothing further and tells its
 * owner: one below the daemon's `launch_failure_limit`, whose default is 3
 * (`packages/daemon/internal/config/config.go:253`), at which the daemon fails the claim. */
export const STALL_EXIT_CAP = 2;

/** The status the worker's last step exits with: an ordinary failure, which the daemon reads as a
 * death and answers by relaunching the claim on the same session with its task still owed. */
export const STALL_EXIT_CODE = 1;

/** One tool call this session has open: its arguments as `tool_call` recorded them, its clock as
 * `tool_execution_start` started it, and how far up the ladder it has been carried. */
export interface StallCall {
  readonly toolCallId: string;
  readonly toolName: string;
  /** The arguments the call was made with, taken at `tool_call`: the one event the host fires
   * before the call can run at all. Never written anywhere durable — the claim row is shared and
   * a shell command routinely holds a credential — only digested by the teller. */
  readonly input: Record<string, unknown>;
  /** When the host handed the call to its tool, which is when it starts being judged; undefined
   * while the call waits in the queue or at the approval gate, where nothing judges it. */
  readonly startedAt: number | undefined;
  /** The deadline this call promised, already clamped as the host clamps it; undefined when it
   * promised none and the role's bound is what judges it. */
  readonly deadlineMs: number | undefined;
  /** How many of `STALL_LADDER`'s steps have run for this call. */
  readonly step: number;
  /** When the last of them ran; the next waits `stepMs` from it. */
  readonly steppedAt: number | undefined;
  /** What the owner's telling did, once per call; undefined before the call was found overdue. */
  readonly told: StallTold | undefined;
}

/** Which rule found a call overdue, with the number it passed. */
export type StallRule =
  | { readonly kind: "deadline"; readonly deadlineMs: number; readonly graceMs: number }
  | { readonly kind: "bound"; readonly boundMs: number };

/** A call that is overdue, and by how much. */
export interface StallOverdue {
  readonly rule: StallRule;
  /** How long the call's execution has been running. */
  readonly openMs: number;
  /** How far past the rule's own number that is. */
  readonly pastMs: number;
}

/** One process of the agent's tree, as the evidence names it. */
export interface StallProcess {
  readonly pid: number;
  readonly command: string;
  /** When the process started, as a wall-clock millisecond. */
  readonly startedAt: number;
  /** The kernel's one-letter state, `R`, `S`, `D`, `Z` and the rest. */
  readonly state: string;
  /** The processor time it has used. */
  readonly cpuMs: number;
  /** The call its own environment names in `PI_TOOL_CALL_ID`, where it names one. */
  readonly toolCallId?: string;
}

/** The agent's process tree, or the fact that it could not be read: a machine whose `/proc` the
 * reader cannot read records that it could not read the tree, never an empty one. */
export type StallProcessTree =
  | { readonly read: true; readonly processes: readonly StallProcess[] }
  | { readonly read: false; readonly reason: string };

/** What one overdue call's owner is told, with the evidence that goes with it. */
export interface StallEvidence {
  readonly call: StallCall;
  readonly overdue: StallOverdue;
  readonly tree: StallProcessTree;
}

/** What the telling achieved. */
export interface StallTold {
  /** The evidence reached the session's transcript. The ladder's last step runs only when it did:
   * a process ended with nothing written behind it is the failure this issue exists to end. */
  readonly evidenceWritten: boolean;
  /** The owner was told. A listener that refuses the publish, and a session with Envoy off, leave
   * this false; the ladder goes on, because an owner that cannot be reached is no reason to leave
   * a worker stuck. */
  readonly ownerTold: boolean;
}

/** One step of the ladder, recorded with what it did. */
export interface StallStepRecord {
  readonly call: StallCall;
  readonly step: StallStep;
  /** How long the call had been running when the step ran. */
  readonly openMs: number;
  /** One line saying what the step did, for the transcript and the log. */
  readonly outcome: string;
}

/** The agent's process tree, and the ending of one call's own processes: `packages/pi-legion`
 * implements both over `/proc`, reading other pids only (measurement 2). */
export interface StallWatchdogProcesses {
  readonly tree: () => Promise<StallProcessTree> | StallProcessTree;
  /** Ends the processes that belong to this call, and answers how many it ended. */
  readonly endCall: (call: StallCall) => Promise<number> | number;
}

/** The evidence and the owner's notice (R3), and the record of each step the ladder ran. */
export interface StallWatchdogTeller {
  /** Writes the evidence to the session's transcript and tells the owner, once per call, before
   * any step runs. */
  readonly tellOverdue: (evidence: StallEvidence) => Promise<StallTold> | StallTold;
  /** Records what one step did, where the evidence went. */
  readonly recordStep: (record: StallStepRecord) => Promise<void> | void;
  /** The ladder stopped short of a step it would otherwise have run, and the owner must read
   * why: the exit cap, an unreadable or unwritable counter file, unwritten evidence. */
  readonly tellStopped: (call: StallCall, reason: string) => Promise<void> | void;
}

/** The host's `abort()` and the message the worker then sends its own session. */
export interface StallWatchdogAborter {
  /** `ctx.abort()`: it ends the turn, keeps the process and the session, and ends the call's own
   * children (measured 2026-11-07). */
  readonly abortTurn: () => Promise<void> | void;
  /** Puts one message in front of this same session, which the host turns into a turn because the
   * session is idle once the abort has ended the turn. The daemon counts a task served by an
   * ended turn and sends nothing after it, so this message is what starts the work again. */
  readonly sendMessage: (text: string) => Promise<void> | void;
}

/** The count of this worker's own stall exits, in a file of its `LEGION_STATE_DIR`. */
export interface StallWatchdogExits {
  /** How many this worker has already made; undefined when the file could not be read, which ends
   * no process at all. */
  readonly read: () => Promise<number | undefined> | number | undefined;
  /** Writes and flushes the new count before the process ends; false when it could not be
   * written, which ends no process at all. */
  readonly write: (count: number) => Promise<boolean> | boolean;
}

export interface StallWatchdogLog {
  readonly debug: (message: string, context?: Record<string, unknown>) => void;
  readonly warn: (message: string, context?: Record<string, unknown>) => void;
}

export interface StallWatchdogDeps {
  /** The clock every measurement here is taken from. */
  readonly now: () => number;
  readonly limits: StallLimits;
  /** The host's `ctx.isIdle()`: no step runs while it reports the session idle. A host that does
   * not answer is read as not idle, which is what a session running a stuck call is. */
  readonly isIdle: () => boolean;
  readonly processes: StallWatchdogProcesses;
  readonly teller: StallWatchdogTeller;
  readonly aborter: StallWatchdogAborter;
  readonly exits: StallWatchdogExits;
  /** Ends this process (`process.exit` behind `packages/pi-legion`'s test seam). */
  readonly exitProcess: (code: number) => void;
  readonly log: StallWatchdogLog;
}

export interface StallWatchdog {
  /** `tool_call`: records the call's arguments. It returns at once — the host holds the whole
   * turn for this handler's budget and fails the call closed when it throws or outruns it, both
   * measured — so it blocks nothing, revises nothing and awaits nothing. */
  readonly callRequested: (event: ToolCallEvent) => void;
  /** `tool_execution_start`: the host has handed the call to its tool, so its clock starts here
   * and neither the queue nor the approval gate is counted against it. A call whose `tool_call`
   * this watch never saw is recorded from this event, with the arguments it carries. */
  readonly executionStarted: (event: ToolExecutionStartEvent) => void;
  /** `tool_execution_end`: closes the record, the error end of a call the host failed closed
   * before it ran among them. */
  readonly executionEnded: (event: ToolExecutionEndEvent) => void;
  /** `tool_result`: closes the record. */
  readonly resultDelivered: (event: ToolResultEvent) => void;
  /** An assistant message from `message_end` or `turn_end`: one that ends aborted closes every
   * open record, since the host ended the turn those calls belonged to. */
  readonly assistantMessage: (message: unknown) => void;
  /** `turn_end`: the turn is over, so every call it held open is closed and no step runs for one
   * of them. */
  readonly turnEnded: () => void;
  /** `session_switch`, `session_branch`, `session_tree`, `session_shutdown`: this session's calls
   * are not this watch's business any more. */
  readonly sessionChanged: () => void;
  /** The 5-second sweep: it judges every open call and carries each overdue one one step. */
  readonly sweep: () => Promise<void>;
  /** The calls open now, newest last. */
  readonly openCalls: () => readonly StallCall[];
}

/**
 * The deadline a call of `toolName` with these arguments promised, in milliseconds, already
 * clamped as the host clamps it; undefined when it promised none, which is a tool whose deadline
 * the host may pause, a tool that takes no deadline, and a call that declares 0, for which the
 * host arms none.
 */
export function effectiveDeadlineMs(
  toolName: string,
  input: Record<string, unknown>
): number | undefined {
  if (STALL_PAUSED_DEADLINE_TOOLS.includes(toolName)) return undefined;
  const tool = STALL_DEADLINE_TOOLS[toolName];
  if (tool === undefined) return undefined;
  const declared = input[tool.argument];
  if (typeof declared !== "number" || !Number.isFinite(declared)) return tool.defaultMs;
  if (declared <= 0) return undefined;
  return Math.min(
    Math.max(declared * 1_000, DECLARED_DEADLINE_LEAST_MS),
    DECLARED_DEADLINE_MOST_MS
  );
}

/**
 * Whether this call has missed a beat at `now`, and by how much. It has not while its clock has
 * not started, while `callBoundMs` is 0, which is the watch switched off for this worker, and
 * whenever it is still inside the rule that judges it: its own deadline plus `callGraceMs` where
 * it promised one, and its role's bound where it promised none.
 */
export function callOverdue(
  call: StallCall,
  now: number,
  limits: StallLimits
): StallOverdue | undefined {
  if (call.startedAt === undefined) return undefined;
  const openMs = now - call.startedAt;
  if (call.deadlineMs !== undefined) {
    const allowed = call.deadlineMs + limits.callGraceMs;
    const rule: StallRule = {
      kind: "deadline",
      deadlineMs: call.deadlineMs,
      graceMs: limits.callGraceMs,
    };
    return openMs > allowed ? { rule, openMs, pastMs: openMs - allowed } : undefined;
  }
  if (limits.callBoundMs === 0) return undefined;
  return openMs > limits.callBoundMs
    ? {
        rule: { kind: "bound", boundMs: limits.callBoundMs },
        openMs,
        pastMs: openMs - limits.callBoundMs,
      }
    : undefined;
}

/** The message the worker sends its own session once the abort has ended the turn: it names the
 * call that was ended, so the next turn knows what did not finish. */
export function restartMessage(call: StallCall, overdue: StallOverdue): string {
  const seconds = Math.round(overdue.openMs / 1_000);
  return (
    `Your turn was ended because its \`${call.toolName}\` call (${call.toolCallId}) had been ` +
    `running ${seconds} seconds with no result, past ${ruleText(overdue.rule)}. Nothing else was ` +
    "touched. Decide whether that call is still worth making, and go on with your work."
  );
}

/** The rule a notice names, in words. */
function ruleText(rule: StallRule): string {
  return rule.kind === "deadline"
    ? `its own ${Math.round(rule.deadlineMs / 1_000)}-second deadline and a ` +
        `${Math.round(rule.graceMs / 1_000)}-second grace`
    : `this role's ${Math.round(rule.boundMs / 1_000)}-second bound`;
}

/** Whether this message is an assistant reply that ended aborted (`stopReason: "aborted"`), which
 * is where an aborted turn is readable: `agent_end` carries no `stopReason` (measured). */
function assistantAborted(message: unknown): boolean {
  if (typeof message !== "object" || message === null) return false;
  const { role, stopReason } = message as { role?: unknown; stopReason?: unknown };
  return role === "assistant" && stopReason === "aborted";
}

/** The record one call's first event opens, with the clock that event starts it at: `tool_call`
 * leaves it unstarted, since nothing judges a call waiting in the queue or at the approval gate,
 * and a `tool_execution_start` whose `tool_call` this watch never saw starts it at once. */
function openedCall(
  event: ToolCallEvent | ToolExecutionStartEvent,
  startedAt: number | undefined
): StallCall {
  return {
    toolCallId: event.toolCallId,
    toolName: event.toolName,
    input: event.input,
    startedAt,
    deadlineMs: effectiveDeadlineMs(event.toolName, event.input),
    step: 0,
    steppedAt: undefined,
    told: undefined,
  };
}

export function createStallWatchdog(deps: StallWatchdogDeps): StallWatchdog {
  /** The open calls, by the id that is the only thing pairing a call's events. Closing one deletes
   * it, which clears its ladder: a later stuck call starts again at the first step. */
  const calls = new Map<string, StallCall>();
  /** Whether a turn is running. No step runs for a call of a turn that is over. */
  let turnOpen = false;
  /** One sweep at a time: a step can outlast the 5-second tick that armed it. */
  let sweeping = false;

  function close(toolCallId: string, why: string): void {
    if (!calls.delete(toolCallId)) return;
    deps.log.debug("legion: the stall watch closed a tool call's record", { toolCallId, why });
  }

  function closeAll(why: string): void {
    if (calls.size === 0) return;
    deps.log.debug("legion: the stall watch closed every open tool call", {
      why,
      open: calls.size,
    });
    calls.clear();
  }

  async function tell(call: StallCall, overdue: StallOverdue): Promise<StallCall> {
    // A reader that throws is a tree that could not be read, which is what the evidence says:
    // nothing about the owner's notice is worth losing over it.
    let tree: StallProcessTree;
    try {
      tree = await deps.processes.tree();
    } catch (error) {
      tree = { read: false, reason: messageFor(error) };
    }
    let told: StallTold;
    try {
      told = await deps.teller.tellOverdue({ call, overdue, tree });
    } catch (error) {
      told = { evidenceWritten: false, ownerTold: false };
      deps.log.warn("legion: the stall watch could not tell the owner of an overdue call", {
        toolCallId: call.toolCallId,
        error: messageFor(error),
      });
    }
    return { ...call, told };
  }

  async function record(call: StallCall, step: StallStep, openMs: number, outcome: string) {
    try {
      await deps.teller.recordStep({ call, step, openMs, outcome });
    } catch (error) {
      deps.log.warn("legion: the stall watch could not record a step", {
        toolCallId: call.toolCallId,
        step,
        error: messageFor(error),
      });
    }
  }

  async function stopped(call: StallCall, reason: string): Promise<void> {
    try {
      await deps.teller.tellStopped(call, reason);
    } catch (error) {
      deps.log.warn("legion: the stall watch could not tell the owner why it stopped", {
        toolCallId: call.toolCallId,
        reason,
        error: messageFor(error),
      });
    }
  }

  /** Step one: end the stuck call's own processes. The turn, the process and the session go on. */
  async function endProcesses(call: StallCall, overdue: StallOverdue): Promise<void> {
    let outcome: string;
    try {
      const ended = await deps.processes.endCall(call);
      outcome = `ended ${ended} process${ended === 1 ? "" : "es"} of the call`;
    } catch (error) {
      outcome = `the call's processes could not be ended: ${messageFor(error)}`;
    }
    await record(call, "end-processes", overdue.openMs, outcome);
  }

  /** Step two: end the turn, and start the work again. Each failure path is defined, because a
   * ladder whose error path leaves no next step is the failure three reviews of the abandoned
   * branch found: the ladder goes on to step three at the next gap either way. */
  async function abortTurn(call: StallCall, overdue: StallOverdue): Promise<void> {
    const outcomes: string[] = [];
    try {
      await deps.aborter.abortTurn();
      outcomes.push("the turn was ended with the host's abort()");
    } catch (error) {
      outcomes.push(`the host's abort() failed: ${messageFor(error)}`);
    }
    try {
      await deps.aborter.sendMessage(restartMessage(call, overdue));
      outcomes.push("the session was sent the message naming the ended call");
    } catch (error) {
      outcomes.push(`the session refused the message naming the ended call: ${messageFor(error)}`);
    }
    await record(call, "abort-turn", overdue.openMs, outcomes.join("; "));
  }

  /** Step three, and only where the abort did not end the turn: end the process, so the daemon
   * reads a death and relaunches the claim on the same session with its task still owed. */
  async function exitProcess(call: StallCall, overdue: StallOverdue): Promise<void> {
    /** The tail every branch that ends nothing shares: the step is recorded with why it ended
     * nothing, and the owner is told the ladder stopped short. */
    const stopShort = async (reason: string): Promise<void> => {
      await record(call, "exit-process", overdue.openMs, reason);
      await stopped(call, reason);
    };
    if (call.told?.evidenceWritten !== true) {
      return stopShort("the evidence for this call was not written, so the process was not ended");
    }
    const made = await deps.exits.read();
    if (made === undefined) {
      return stopShort("the stall exit count could not be read, so the process was not ended");
    }
    if (made >= STALL_EXIT_CAP) {
      return stopShort(
        `this worker has already ended itself ${made} times for a stuck call, which is its cap ` +
          `of ${STALL_EXIT_CAP}; the process was not ended`
      );
    }
    if (!(await deps.exits.write(made + 1))) {
      return stopShort("the stall exit count could not be written, so the process was not ended");
    }
    await record(
      call,
      "exit-process",
      overdue.openMs,
      `ending this process, stall exit ${made + 1} of ${STALL_EXIT_CAP}`
    );
    deps.exitProcess(STALL_EXIT_CODE);
  }

  /** One overdue call, one step. The record is read again from the map at every await boundary:
   * an event may have closed the call while a step ran, and a closed call is left alone. */
  async function advance(toolCallId: string, now: number): Promise<void> {
    const open = calls.get(toolCallId);
    if (open === undefined) return;
    const overdue = callOverdue(open, now, deps.limits);
    if (overdue === undefined) return;

    let call = open;
    if (call.told === undefined) {
      call = await tell(call, overdue);
      if (!calls.has(toolCallId)) return;
      calls.set(toolCallId, call);
    } else if (call.steppedAt !== undefined && now - call.steppedAt < deps.limits.stepMs) {
      return;
    }

    const step = STALL_LADDER[call.step];
    if (step === undefined) return;
    if (step === "end-processes") await endProcesses(call, overdue);
    else if (step === "abort-turn") await abortTurn(call, overdue);
    else await exitProcess(call, overdue);

    const after = calls.get(toolCallId);
    if (after === undefined) return;
    calls.set(toolCallId, { ...after, step: call.step + 1, steppedAt: now });
  }

  return {
    callRequested: (event) => {
      // A call is made inside a turn, so one seen here says a turn runs even where the entry
      // registered no turn_start of its own.
      turnOpen = true;
      calls.set(event.toolCallId, openedCall(event, undefined));
    },

    executionStarted: (event) => {
      turnOpen = true;
      const now = deps.now();
      const open = calls.get(event.toolCallId);
      if (open === undefined) {
        calls.set(event.toolCallId, openedCall(event, now));
        return;
      }
      calls.set(event.toolCallId, { ...open, startedAt: now });
    },

    executionEnded: (event) => close(event.toolCallId, "tool_execution_end"),
    resultDelivered: (event) => close(event.toolCallId, "tool_result"),
    assistantMessage: (message) => {
      if (assistantAborted(message)) closeAll("an assistant message that ended aborted");
    },
    turnEnded: () => {
      turnOpen = false;
      closeAll("turn_end");
    },
    sessionChanged: () => {
      turnOpen = false;
      closeAll("a session switch or shutdown");
    },

    sweep: async () => {
      if (sweeping || !turnOpen || calls.size === 0) return;
      if (deps.isIdle()) return;
      const now = deps.now();
      const overdue = [...calls.values()]
        .filter((call) => callOverdue(call, now, deps.limits) !== undefined)
        .map((call) => call.toolCallId);
      if (overdue.length === 0) return;
      sweeping = true;
      try {
        for (const toolCallId of overdue) {
          // One call's step never costs another its sweep, and the host's managed interval is
          // never handed a rejection.
          try {
            await advance(toolCallId, now);
          } catch (error) {
            deps.log.warn("legion: the stall watch could not carry a stuck call one step", {
              toolCallId,
              error: messageFor(error),
            });
          }
        }
      } finally {
        sweeping = false;
      }
    },

    openCalls: () => [...calls.values()],
  };
}

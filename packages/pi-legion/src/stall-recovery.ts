import { messageFor } from "@legion/envoy-client/errors";
import type {
  StallCall,
  StallProcessTree,
  StallWatchdogLog,
  StallWatchdogProcesses,
} from "@legion/pi-shared/stall-watchdog";
import {
  readStallProcesses,
  type StallProcessScan,
  type StallScannedProcess,
  stallProcessTree,
} from "./stall-evidence";

/**
 * The first step of the stall ladder (INFRA-1274, R4): the stuck call's own processes, ended.
 * `packages/pi-shared/src/stall-watchdog.ts` decides when the step runs; this module decides
 * which processes the step is about, and it is the one place that ends anything.
 *
 * **How it finds them**, in the order it tries, each only because the one before it found
 * nothing:
 *
 * 1. **A process whose environment names the call.** `PI_TOOL_CALL_ID` is exactly the
 *    `toolCallId` the call carries, on a shell command's own process and on its children
 *    (measured 2026-11-07, `omp/18.16.0`).
 * 2. **A descendant of the agent that started at or after the call's execution.** A call that
 *    forked something the host did not stamp still started it, and the clock is what says so.
 * 3. **The tool's own runner for this session**, where exactly one call of that tool is open and
 *    the host holds no background job of that tool. A runner is kept between calls, so it is the
 *    one thing here older than the call that may still be ended: the `eval` kernel is
 *    `/usr/bin/python3 -u <tmp>/omp-python-runner-<uid>/runner-<id>.py`, one process per session,
 *    and it names the session it serves in `PI_SESSION_FILE` — never in `PI_SESSION_ID`, which
 *    it does not carry (measured). A runner whose environment names no session, or names another,
 *    is left alone, which is how a subagent's own runner of the same command, with the same
 *    parent and the same working directory, is spared.
 *
 * **Three things it never ends.**
 *
 * - **The agent's own process.** A plain `bash` command has no process of its own: the shell runs
 *   inside Oh My Pi, a command's `$$` is the agent's own pid and `sleep` is a builtin (measured),
 *   so a rule that ended "the call's process" would end the worker itself.
 * - **Anything the agent started before the call.** That is what bounds rule 2 to the processes
 *   whose clock puts them inside the call; rule 1 is the call's own stamp, whatever the clock
 *   says, and rule 3 is the single documented exception, a runner the tool keeps between calls.
 * - **Anything whose environment names another call**, whatever became of that call. A process
 *   inherits its environment, so every process of a call the host has handed to its background
 *   job manager still carries that call's id: the host's background jobs are spared by this one
 *   rule, and rule 3 asks the host's job manager as well, because a backgrounded `eval` has
 *   closed its call's record while still using the runner.
 *
 * A process the kernel holds as a zombie has already ended and is waiting to be reaped, so it is
 * not ended again and is not counted: that is what keeps the step's own count the number of
 * processes it really ended.
 */

/** The runner one tool keeps between calls, by tool name, and the command that identifies one.
 * `eval`'s is the Python kernel, one process per session, a direct child of the agent: measured
 * on 2026-11-07 against `omp/18.16.0` in this deployment's pod as
 * `/usr/bin/python3 -u /tmp/omp-python-runner-1000/runner-2gcnasrh1f37g.py`, a directory named
 * for the user holding a file named for the kernel, and recorded earlier the same day in the flat
 * `<tmp>/omp-python-runner-<id>.py` shape. The pattern spans both, because a path separator
 * between the two is exactly what the live proof found it refusing
 * (extensions/legion-stall-watchdog-omp.test.ts). A tool with no entry has no runner this module
 * knows, so rule 3 never runs for it. */
export const STALL_TOOL_RUNNERS: Readonly<Record<string, RegExp>> = {
  eval: /omp-python-runner-[^\s]*\.py/,
};

/** Which rule found the processes a step ended, or why it found none. */
export type StallCallProcesses =
  | {
      readonly rule: "named-call" | "started-after" | "tool-runner";
      readonly pids: readonly number[];
    }
  | { readonly rule: "none"; readonly why: string };

export interface StallCallProcessesInput {
  readonly call: StallCall;
  readonly scan: StallProcessScan;
  /** The agent's own process, which is the root of the tree and is never ended. */
  readonly agentPid: number;
  /** The session file this session's tool runners name in `PI_SESSION_FILE`; undefined where the
   * host has not said, which leaves every runner alone. */
  readonly sessionFile: string | undefined;
  /** Every call this session has open now. */
  readonly openCalls: readonly StallCall[];
  /** What the host still runs as a background job of this session. */
  readonly backgroundJobs: readonly { readonly type: string }[];
}

/**
 * The processes of one stuck call: the three rules, in order, and the exclusions applied to
 * whatever they find. Pure, so the rules are read and tested apart from the ending.
 */
export function stallCallProcesses(input: StallCallProcessesInput): StallCallProcesses {
  const { call, scan, agentPid } = input;
  if (!scan.read)
    return { rule: "none", why: `the process tree could not be read: ${scan.reason}` };
  const byPid = new Map(scan.processes.map((process) => [process.pid, process]));

  /** A process is never ended when it is the agent itself, when its environment names a call
   * other than this one — whatever became of that call — or when the kernel has it as a zombie,
   * which is a process that has already ended and is waiting to be reaped. */
  const sparing = (process: StallScannedProcess): string | undefined => {
    if (process.pid === agentPid) return "it is the agent's own process";
    if (process.state === "Z") return "it has already ended";
    const named = process.environment.PI_TOOL_CALL_ID;
    if (named !== undefined && named !== call.toolCallId) return `its environment names ${named}`;
    return undefined;
  };

  /** Everything below a process, so a call's grandchildren go with it; each is spared on its own
   * terms, so nothing of another call is taken along. */
  const withDescendants = (pids: readonly number[]): number[] => {
    const taken = new Set<number>(pids);
    for (const process of scan.processes) {
      for (let at = byPid.get(process.ppid); at !== undefined; at = byPid.get(at.ppid)) {
        if (!taken.has(at.pid)) continue;
        taken.add(process.pid);
        break;
      }
    }
    return [...taken].filter((pid) => {
      const process = byPid.get(pid);
      return process !== undefined && sparing(process) === undefined;
    });
  };

  const named = scan.processes.filter(
    (process) => process.environment.PI_TOOL_CALL_ID === call.toolCallId && process.pid !== agentPid
  );
  if (named.length > 0) {
    return { rule: "named-call", pids: withDescendants(named.map((process) => process.pid)) };
  }

  const startedAt = call.startedAt;
  if (startedAt !== undefined) {
    const after = scan.processes.filter(
      (process) => process.pid !== agentPid && process.startedAt >= startedAt
    );
    const pids = withDescendants(after.map((process) => process.pid));
    if (pids.length > 0) return { rule: "started-after", pids };
  }

  const runner = STALL_TOOL_RUNNERS[call.toolName];
  if (runner === undefined) {
    return {
      rule: "none",
      why: `no process names this call and \`${call.toolName}\` keeps no runner`,
    };
  }
  const openOfTool = input.openCalls.filter((open) => open.toolName === call.toolName);
  if (openOfTool.length !== 1) {
    return {
      rule: "none",
      why: `${openOfTool.length} calls of \`${call.toolName}\` are open, so no runner is this call's`,
    };
  }
  if (input.backgroundJobs.some((job) => job.type === call.toolName)) {
    return {
      rule: "none",
      why: `the host still holds a background \`${call.toolName}\` job, which the runner is serving`,
    };
  }
  if (input.sessionFile === undefined) {
    return {
      rule: "none",
      why: "this session's own file is unknown, so no runner is known to be its",
    };
  }
  const runners = scan.processes.filter(
    (process) =>
      process.pid !== agentPid &&
      runner.test(process.command) &&
      process.environment.PI_SESSION_FILE === input.sessionFile
  );
  const [only] = runners;
  if (runners.length !== 1 || only === undefined) {
    return {
      rule: "none",
      why: `${runners.length} \`${call.toolName}\` runners of this session were found, not one`,
    };
  }
  return { rule: "tool-runner", pids: withDescendants([only.pid]) };
}

export interface StallProcessesDeps {
  /** The agent's own process; `process.pid` unless a test names another. */
  readonly agentPid?: number;
  /** Where the kernel's process table is mounted; `/proc` unless a test points it elsewhere. */
  readonly procRoot?: string;
  /** The file this session is persisted to, which its tool runners carry in `PI_SESSION_FILE`
   * (`persistedTranscript`, `extensions/legion.ts:105-116`); undefined before the host has said,
   * which leaves every runner alone. */
  readonly sessionFile: () => string | undefined;
  /** Every call this session has open now (`StallWatchdog.openCalls`). */
  readonly openCalls: () => readonly StallCall[];
  /** The background jobs the host still runs for this session, as
   * `ctx.getAsyncJobSnapshot()?.running` reports them; empty where the host has none or answers
   * nothing. */
  readonly backgroundJobs: () => readonly { readonly type: string }[];
  readonly log: StallWatchdogLog;
}

/**
 * The detector's `StallWatchdogProcesses`: the agent's tree for the evidence
 * (`src/stall-evidence.ts`), and the ending of one call's own processes for the ladder's first
 * step. One object, because both halves read the same `/proc` the same way.
 */
export function createStallProcesses(deps: StallProcessesDeps): StallWatchdogProcesses {
  const scan = async (): Promise<StallProcessScan> =>
    readStallProcesses({
      ...(deps.agentPid === undefined ? {} : { agentPid: deps.agentPid }),
      ...(deps.procRoot === undefined ? {} : { procRoot: deps.procRoot }),
    });

  return {
    tree: async (): Promise<StallProcessTree> => stallProcessTree(await scan()),

    endCall: async (call: StallCall): Promise<number> => {
      const found = stallCallProcesses({
        call,
        scan: await scan(),
        agentPid: deps.agentPid ?? process.pid,
        sessionFile: deps.sessionFile(),
        openCalls: deps.openCalls(),
        backgroundJobs: deps.backgroundJobs(),
      });
      if (found.rule === "none") {
        deps.log.debug("legion: the stall watch found no process of this stuck call", {
          toolCallId: call.toolCallId,
          tool: call.toolName,
          why: found.why,
        });
        return 0;
      }
      let ended = 0;
      // The scan lists a parent before its descendants, so reversing ends the deepest of a
      // subtree first.
      for (const pid of [...found.pids].reverse()) {
        try {
          process.kill(pid, "SIGKILL");
          ended += 1;
        } catch (error) {
          // A process that ended between the scan and the signal is one fewer to end, not a
          // failure of the step.
          deps.log.debug("legion: the stall watch did not end a process of a stuck call", {
            toolCallId: call.toolCallId,
            pid,
            error: messageFor(error),
          });
        }
      }
      deps.log.warn("legion: the stall watch ended the processes of a stuck tool call", {
        toolCallId: call.toolCallId,
        tool: call.toolName,
        rule: found.rule,
        pids: found.pids,
        ended,
      });
      return ended;
    },
  };
}

import { afterAll, expect, test } from "bun:test";
import { mkdtempSync, rmSync, writeFileSync } from "node:fs";
import { tmpdir } from "node:os";
import path from "node:path";
import type { StallCall, StallWatchdogLog } from "@legion/pi-shared/stall-watchdog";
import {
  readStallProcesses,
  type StallProcessScan,
  type StallScannedProcess,
} from "./stall-evidence";
// The import above is this file's merge-base check: on the merge base there is no
// `./stall-recovery`, so every test here fails to resolve rather than passing vacuously.
import { createStallProcesses, STALL_TOOL_RUNNERS, stallCallProcesses } from "./stall-recovery";

/**
 * The ladder's first step against a real process tree in this pod: the processes are spawned for
 * real, with the environments the measurement found on them, the step runs, and the machine is
 * read again to see which of them are alive. Nothing here fakes `/proc`.
 */

const SESSION_FILE = "/legion/scratch/tmp/stall-recovery-session.jsonl";
const OTHER_SESSION_FILE = "/legion/scratch/tmp/stall-recovery-subagent.jsonl";

/** Everything this file started, so nothing outlives it: a child of the agent survives the
 * agent's death reparented to pid 1 (measured), so each is ended by pid. */
const started: number[] = [];
const scratch = mkdtempSync(path.join(process.env.TMPDIR ?? tmpdir(), "stall-recovery-"));

afterAll(() => {
  for (const pid of started) {
    try {
      process.kill(pid, "SIGKILL");
    } catch {
      // Already gone, which is what most of them are.
    }
  }
  rmSync(scratch, { recursive: true, force: true });
});

const log: StallWatchdogLog = { debug: () => {}, warn: () => {} };

/** Whether this pid is a process that is still running: a pid whose parent has not reaped it is
 * a `Z` in `/proc`, which is ended as far as this step is concerned. */
async function alive(pid: number): Promise<boolean> {
  const stat = await Bun.file(`/proc/${pid}/stat`)
    .text()
    .catch(() => "");
  const state = stat
    .slice(stat.lastIndexOf(")") + 1)
    .trim()
    .split(/\s+/)[0];
  return state !== undefined && state !== "" && state !== "Z";
}

/** Waits for one reading of the machine to come true. A real tree has no clock to fake, so what
 * is awaited is the reading itself, never a guessed delay. */
async function until(ready: () => Promise<boolean>): Promise<void> {
  for (let attempt = 0; attempt < 1_000; attempt += 1) {
    if (await ready()) return;
    await Bun.sleep(10);
  }
  throw new Error("the machine never reached the state the case needs");
}

function callOf(over: Partial<StallCall> = {}): StallCall {
  return {
    toolCallId: "toolu_stuck",
    toolName: "eval",
    input: { code: "while True: pass" },
    startedAt: undefined,
    deadlineMs: undefined,
    step: 0,
    steppedAt: undefined,
    told: undefined,
    ...over,
  };
}

test("the first step ends the stuck call's own processes, and only those", async () => {
  // The two runners of the measured shape: one process per session, `/usr/bin/python3 -u
  // <tmp>/omp-python-runner-<id>.py`, naming the session it serves in PI_SESSION_FILE.
  const ownRunner = path.join(scratch, "omp-python-runner-own.py");
  const subRunner = path.join(scratch, "omp-python-runner-subagent.py");
  for (const file of [ownRunner, subRunner]) {
    writeFileSync(file, "import time\ntime.sleep(300)\n");
  }

  // The agent, with no PI_* name in its own environment: this test runs inside a tool call of
  // its own, and an inherited PI_TOOL_CALL_ID would make every child look like another call's.
  const clean = {
    PATH: process.env.PATH ?? "/usr/bin:/bin",
    TMPDIR: process.env.TMPDIR ?? tmpdir(),
  };
  const agent = Bun.spawn(
    [
      "sh",
      "-c",
      [
        // Wave one: what the agent started before the call. The runners are among them,
        // because a runner is what the tool keeps between calls.
        `PI_SESSION_FILE='${SESSION_FILE}' /usr/bin/python3 -u '${ownRunner}' & echo own-runner $!`,
        `PI_SESSION_FILE='${OTHER_SESSION_FILE}' /usr/bin/python3 -u '${subRunner}' & echo sub-runner $!`,
        "sleep 300 & echo before $!",
        // The gap the call's execution is placed in.
        "sleep 1",
        // Wave two: what started inside the call.
        "PI_TOOL_CALL_ID=toolu_stuck sleep 300 & echo stuck $!",
        "PI_TOOL_CALL_ID=toolu_other sleep 300 & echo other $!",
        "PI_TOOL_CALL_ID=toolu_ended sleep 300 & echo ended $!",
        "sleep 300 & echo plain $!",
        "echo ready",
        // A builtin wait, so the agent forks nothing more of its own.
        "read _end",
      ].join("\n"),
    ],
    { env: clean, stdin: "pipe", stdout: "pipe", stderr: "ignore" }
  );
  started.push(agent.pid);

  // One that is no descendant of the agent, naming the stuck call all the same.
  const outsider = Bun.spawn(["sh", "-c", "exec sleep 300"], {
    env: { ...clean, PI_TOOL_CALL_ID: "toolu_stuck" },
    stdio: ["ignore", "ignore", "ignore"],
  });
  started.push(outsider.pid);

  // The agent prints one line per process it started, then `ready`. The chunks a pipe hands
  // over do not respect lines, so the whole answer is gathered before it is read.
  const pids = new Map<string, number>();
  let printed = "";
  for await (const chunk of agent.stdout as ReadableStream<Uint8Array>) {
    printed += new TextDecoder().decode(chunk);
    if (printed.includes("ready")) break;
  }
  for (const row of printed.split("\n")) {
    const [name, pid] = row.trim().split(/\s+/);
    if (name === undefined || pid === undefined) continue;
    pids.set(name, Number.parseInt(pid, 10));
    started.push(Number.parseInt(pid, 10));
  }
  const pidOf = (name: string): number => {
    const pid = pids.get(name);
    if (pid === undefined) throw new Error(`the agent never started ${name}`);
    return pid;
  };
  const everyone = ["own-runner", "sub-runner", "before", "stuck", "other", "ended", "plain"];
  expect(everyone.filter((name) => pids.has(name))).toEqual(everyone);

  // Every one of them is in the agent's tree, with the environment its case needs.
  await until(async () => {
    const scan = await readStallProcesses({ agentPid: agent.pid });
    if (!scan.read) return false;
    const found = new Map(scan.processes.map((process) => [process.pid, process]));
    return (
      everyone.every((name) => found.has(pidOf(name))) &&
      found.get(pidOf("stuck"))?.toolCallId === "toolu_stuck" &&
      found.get(pidOf("own-runner"))?.environment.PI_SESSION_FILE === SESSION_FILE &&
      found.get(pidOf("sub-runner"))?.environment.PI_SESSION_FILE === OTHER_SESSION_FILE &&
      found.get(pidOf("plain"))?.toolCallId === undefined
    );
  });

  const scan = await readStallProcesses({ agentPid: agent.pid });
  if (!scan.read) throw new Error("this pod's /proc could not be read");
  const startedAt = (name: string): number =>
    scan.processes.find((process) => process.pid === pidOf(name))?.startedAt as number;
  // The call's execution is the gap between the two waves: everything of wave two started at
  // or after it, everything of wave one before it.
  const execution = Math.min(startedAt("stuck"), startedAt("other"), startedAt("plain"));
  expect(Math.max(startedAt("own-runner"), startedAt("before"))).toBeLessThan(execution);

  // The subagent's own runner has the same command, the same parent and the same working
  // directory as this session's, and a session file of its own.
  const commandOf = (name: string): string =>
    scan.processes.find((process) => process.pid === pidOf(name))?.command as string;
  expect(commandOf("sub-runner").replace("subagent", "own")).toBe(commandOf("own-runner"));
  expect(STALL_TOOL_RUNNERS.eval?.test(commandOf("sub-runner"))).toBe(true);

  let backgroundJobs: { readonly type: string }[] = [];
  let openCalls: StallCall[] = [];
  const processes = createStallProcesses({
    agentPid: agent.pid,
    sessionFile: () => SESSION_FILE,
    openCalls: () => openCalls,
    backgroundJobs: () => backgroundJobs,
    log,
  });

  const stuck = callOf({ startedAt: execution });
  const other = callOf({ toolCallId: "toolu_other", toolName: "bash", startedAt: execution });
  openCalls = [stuck, other];

  // Step one, first rule: the child whose environment names the stuck call, and nothing else.
  expect(await processes.endCall(stuck)).toBe(1);
  await until(async () => !(await alive(pidOf("stuck"))));
  expect(await alive(pidOf("plain"))).toBe(true);

  // With no such child, a descendant that started at or after the call's execution.
  const nameless = callOf({ toolCallId: "toolu_nameless", startedAt: execution });
  openCalls = [nameless, other];
  expect(await processes.endCall(nameless)).toBe(1);
  await until(async () => !(await alive(pidOf("plain"))));

  // With none of those: the runner the tool keeps between calls — but not while the host still
  // holds a background job of that tool, which is what the runner would be serving.
  backgroundJobs = [{ type: "eval" }];
  expect(await processes.endCall(nameless)).toBe(0);
  expect(await alive(pidOf("own-runner"))).toBe(true);

  // Nor while a second call of that tool is open, since neither call owns the runner.
  backgroundJobs = [];
  openCalls = [nameless, callOf({ toolCallId: "toolu_second" })];
  expect(await processes.endCall(nameless)).toBe(0);
  expect(await alive(pidOf("own-runner"))).toBe(true);

  // One call of the tool open, and no background job: the runner of this session goes.
  openCalls = [nameless, other];
  expect(await processes.endCall(nameless)).toBe(1);
  await until(async () => !(await alive(pidOf("own-runner"))));

  // The negative control, at the end of every run: the other open call's child, the background
  // job of the call that ended, the subagent's own runner, the process started before the call
  // and the one that is no descendant are all alive, and so is the agent.
  for (const name of ["other", "ended", "sub-runner", "before"]) {
    expect([name, await alive(pidOf(name))]).toEqual([name, true]);
  }
  expect(await alive(outsider.pid)).toBe(true);
  expect(await alive(agent.pid)).toBe(true);
}, 60_000);

// ---------------------------------------------------------------------------------------------
// The rules, read apart from the machine: what the step would end, and what it never ends.
// ---------------------------------------------------------------------------------------------

function processOf(over: Partial<StallScannedProcess> & { pid: number }): StallScannedProcess {
  return {
    ppid: 100,
    command: "sleep 300",
    startedAt: 1_793_000_000_000,
    state: "S",
    cpuMs: 0,
    environment: {},
    ...over,
  };
}

function scanOf(processes: readonly StallScannedProcess[]): StallProcessScan {
  return { read: true, processes };
}

const base = {
  agentPid: 100,
  sessionFile: SESSION_FILE,
  openCalls: [callOf({ startedAt: 1_793_000_000_000 })],
  backgroundJobs: [],
};

test("the agent's own process is never ended, whatever its environment names", () => {
  // Measured: a plain `bash` command has no process of its own, and its `$$` is the agent's pid,
  // so the agent itself carries the call's own PI_TOOL_CALL_ID.
  const call = callOf({ toolName: "bash", startedAt: 1_793_000_000_000 });
  const found = stallCallProcesses({
    ...base,
    call,
    openCalls: [call],
    scan: scanOf([
      processOf({ pid: 100, ppid: 1, environment: { PI_TOOL_CALL_ID: "toolu_stuck" } }),
    ]),
  });
  expect(found).toEqual({
    rule: "none",
    why: "no process names this call and `bash` keeps no runner",
  });
});

test("a call's grandchildren go with it, and another call's child never does", () => {
  const found = stallCallProcesses({
    ...base,
    call: callOf({ startedAt: 1_793_000_000_000 }),
    scan: scanOf([
      processOf({ pid: 100, ppid: 1 }),
      processOf({ pid: 101, environment: { PI_TOOL_CALL_ID: "toolu_stuck" } }),
      // A grandchild that lost the stamp, and one of another call that did not.
      processOf({ pid: 102, ppid: 101 }),
      processOf({ pid: 103, ppid: 101, environment: { PI_TOOL_CALL_ID: "toolu_other" } }),
    ]),
  });
  expect(found).toEqual({ rule: "named-call", pids: [101, 102] });
});

test("a process already ended is not ended again", () => {
  const found = stallCallProcesses({
    ...base,
    call: callOf({ startedAt: 1_793_000_000_000 }),
    scan: scanOf([
      processOf({ pid: 100, ppid: 1 }),
      processOf({ pid: 101, state: "Z", environment: { PI_TOOL_CALL_ID: "toolu_stuck" } }),
    ]),
  });
  expect(found).toEqual({ rule: "named-call", pids: [] });
});

test("a runner that names another session, or no session, is left alone", () => {
  const runners = scanOf([
    processOf({ pid: 100, ppid: 1 }),
    processOf({
      pid: 101,
      command: "/usr/bin/python3 -u /tmp/omp-python-runner-a.py",
      environment: { PI_SESSION_FILE: OTHER_SESSION_FILE },
    }),
    processOf({ pid: 102, command: "/usr/bin/python3 -u /tmp/omp-python-runner-b.py" }),
  ]);
  // Nothing names the call and nothing started after it, so the runner rule is the one left.
  const call = callOf({ startedAt: 1_793_000_000_001 });
  expect(stallCallProcesses({ ...base, call, openCalls: [call], scan: runners })).toEqual({
    rule: "none",
    why: "0 `eval` runners of this session were found, not one",
  });
});

test("a tree that could not be read ends nothing", async () => {
  const found = stallCallProcesses({
    ...base,
    call: callOf({ startedAt: 1_793_000_000_000 }),
    scan: { read: false, reason: "/proc could not be read: ENOENT" },
  });
  expect(found).toEqual({
    rule: "none",
    why: "the process tree could not be read: /proc could not be read: ENOENT",
  });

  const absent = path.join(scratch, "no-proc-here");
  const processes = createStallProcesses({
    agentPid: 1,
    procRoot: absent,
    sessionFile: () => SESSION_FILE,
    openCalls: () => [],
    backgroundJobs: () => [],
    log,
  });
  expect(await processes.endCall(callOf({ startedAt: 1_793_000_000_000 }))).toBe(0);
  expect(await processes.tree()).toEqual({
    read: false,
    reason: `${absent} could not be read: ENOENT: no such file or directory, scandir '${absent}'`,
  });
});

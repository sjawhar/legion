import { afterEach, expect, test } from "bun:test";
import { mkdirSync, mkdtempSync, readFileSync, rmSync, symlinkSync, writeFileSync } from "node:fs";
import { tmpdir } from "node:os";
import path from "node:path";
import { DEFAULT_STALL_LIMITS, defaultCallBoundMs } from "@legion/pi-shared/stall-limits";
import {
  createStallWatchdog,
  STALL_EXIT_CAP,
  STALL_EXIT_CODE,
  type StallCall,
  type StallStepRecord,
  type StallWatchdog,
  type StallWatchdogExits,
  type StallWatchdogLog,
  type StallWatchdogTeller,
} from "@legion/pi-shared/stall-watchdog";
import type { LegionSessionKind } from "./classify";
import { createStallTeller } from "./stall-evidence";
// The import below is this file's merge-base check: on the merge base there is no `./stall-exits`,
// so every test here fails to resolve rather than passing vacuously.
import { createStallExits, type StallExitsRecord, stallExitsFile } from "./stall-exits";

const SECOND = 1_000;
const MINUTE = 60 * SECOND;
const START = 1_793_000_000_000;

const WORKER: LegionSessionKind = {
  kind: "phase-worker",
  role: "implementer",
  tree: "INFRA-1200",
  issue: "INFRA-1274",
};

const directories: string[] = [];

afterEach(() => {
  for (const directory of directories.splice(0))
    rmSync(directory, { recursive: true, force: true });
});

/** One pod's `LEGION_STATE_DIR`, removed when the case ends. */
function stateDir(): string {
  const directory = mkdtempSync(path.join(tmpdir(), "legion-stall-exits-"));
  directories.push(directory);
  return directory;
}

interface Warnings extends StallWatchdogLog {
  readonly lines: string[];
}

function warnings(): Warnings {
  const lines: string[] = [];
  return { lines, debug: () => {}, warn: (message) => lines.push(message) };
}

// ---------------------------------------------------------------------------------------------
// The file: whose count it is, what it holds, and what it answers when it cannot be read.
// ---------------------------------------------------------------------------------------------

test("the counter file is the claim's own, so panes sharing a state directory keep their own counts", () => {
  const directory = stateDir();
  expect(stallExitsFile(directory, WORKER)).toBe(
    path.join(directory, "legion-stall-exits-infra-1274-implementer.json")
  );
  expect(stallExitsFile(directory, { kind: "root-architect", tree: "INFRA-1200" })).toBe(
    path.join(directory, "legion-stall-exits-infra-1200-architect.json")
  );
  expect(stallExitsFile(directory, { kind: "controller" })).toBe(
    path.join(directory, "legion-stall-exits-controller.json")
  );

  // Two panes of one pod: the tester's exit is not the implementer's.
  const log = warnings();
  const implementer = createStallExits({
    stateDir: directory,
    session: WORKER,
    now: () => START,
    log,
  });
  const tester = createStallExits({
    stateDir: directory,
    session: { ...WORKER, role: "tester" },
    now: () => START,
    log,
  });
  expect(implementer.write(STALL_EXIT_CAP)).toBe(true);
  expect(implementer.read()).toBe(STALL_EXIT_CAP);
  expect(tester.read()).toBe(0);
});

test("a worker that has ended itself no times reads zero, and the count it wrote survives a relaunch", () => {
  const directory = stateDir();
  const log = warnings();
  const deps = { stateDir: directory, session: WORKER, now: () => START, log };
  const exits = createStallExits(deps);

  // No file at all is a worker that has never ended itself in this pod, which is every first
  // stall, and not a file that could not be read.
  expect(exits.read()).toBe(0);
  expect(exits.write(1)).toBe(true);

  const written = JSON.parse(
    readFileSync(stallExitsFile(directory, WORKER), "utf8")
  ) as StallExitsRecord;
  expect(written).toEqual({
    exits: 1,
    at: new Date(START).toISOString(),
    who: "infra-1274-implementer",
  });

  // The relaunch the daemon makes after this worker's exit is a new process of the same pod: it
  // reads the count its predecessor left.
  expect(createStallExits(deps).read()).toBe(1);
  expect(log.lines).toEqual([]);
});

test("a state directory the worker has yet to make is made by the write", () => {
  const directory = path.join(stateDir(), "state", "nested");
  const exits = createStallExits({
    stateDir: directory,
    session: WORKER,
    now: () => START,
    log: warnings(),
  });
  expect(exits.write(1)).toBe(true);
  expect(exits.read()).toBe(1);
});

test("a file that cannot be read, and one that cannot be written, answer so and end nothing", () => {
  // Unreadable: a directory stands where the counter file belongs, which is a state directory a
  // person or another tool has left in a shape this worker cannot use.
  const unreadableDir = stateDir();
  mkdirSync(stallExitsFile(unreadableDir, WORKER));
  const unreadableLog = warnings();
  const unreadable = createStallExits({
    stateDir: unreadableDir,
    session: WORKER,
    now: () => START,
    log: unreadableLog,
  });
  expect(unreadable.read()).toBeUndefined();
  expect(unreadable.write(1)).toBe(false);
  expect(unreadableLog.lines).toEqual([
    "legion: the stall watch could not read its own exit count",
    "legion: the stall watch could not write its own exit count",
  ]);

  // Unwritable: the counter file is a link to a path that cannot be opened for writing, so the
  // count reads as zero and the write that would carry it fails.
  const unwritableDir = stateDir();
  symlinkSync(
    path.join(unwritableDir, "gone", "count.json"),
    stallExitsFile(unwritableDir, WORKER)
  );
  const unwritableLog = warnings();
  const unwritable = createStallExits({
    stateDir: unwritableDir,
    session: WORKER,
    now: () => START,
    log: unwritableLog,
  });
  expect(unwritable.read()).toBe(0);
  expect(unwritable.write(1)).toBe(false);
  expect(unwritableLog.lines).toEqual([
    "legion: the stall watch could not write its own exit count",
  ]);
});

test("a file holding anything this module did not write is a count the worker will not trust", () => {
  for (const content of [
    '{"exits": "two"}',
    '{"exits": -1}',
    '{"exits": 1.5}',
    "{}",
    "2",
    "",
    "x",
  ]) {
    const directory = stateDir();
    writeFileSync(stallExitsFile(directory, WORKER), content);
    const log = warnings();
    const exits = createStallExits({
      stateDir: directory,
      session: WORKER,
      now: () => START,
      log,
    });
    expect(exits.read(), content).toBeUndefined();
    expect(log.lines, content).toEqual([
      "legion: the stall watch's exit count is not a count it wrote",
    ]);
  }
});

// ---------------------------------------------------------------------------------------------
// Driven by the detector itself: the third step, counted in a real state directory.
// ---------------------------------------------------------------------------------------------

interface LadderRig {
  readonly watchdog: StallWatchdog;
  readonly tick: (ms: number) => void;
  readonly steps: StallStepRecord[];
  readonly stopped: { readonly call: StallCall; readonly reason: string }[];
  readonly exitCodes: number[];
}

/**
 * The detector with the real counter over `exits`, and an abort that does not end the turn:
 * the last step runs only where the step before it left the call open.
 */
function ladder(exits: StallWatchdogExits, teller?: StallWatchdogTeller): LadderRig {
  let clock = START;
  const steps: StallStepRecord[] = [];
  const stopped: { call: StallCall; reason: string }[] = [];
  const exitCodes: number[] = [];

  return {
    watchdog: createStallWatchdog({
      now: () => clock,
      limits: { ...DEFAULT_STALL_LIMITS, callBoundMs: defaultCallBoundMs("tester") },
      isIdle: () => false,
      processes: { tree: () => ({ read: true, processes: [] }), endCall: () => 0 },
      teller: teller ?? {
        tellOverdue: () => ({ evidenceWritten: true, ownerTold: true }),
        recordStep: (record) => {
          steps.push(record);
        },
        tellStopped: (call, reason) => {
          stopped.push({ call, reason });
        },
      },
      aborter: { abortTurn: () => {}, sendMessage: () => {} },
      exits,
      exitProcess: (code) => {
        exitCodes.push(code);
      },
      log: { debug: () => {}, warn: () => {} },
    }),
    tick: (ms) => {
      clock += ms;
    },
    steps,
    stopped,
    exitCodes,
  };
}

/** One stuck call carried the whole ladder: the PTY shell call this issue is about, overdue by
 * its own deadline and its grace, and a step gap between each step. */
async function stall(rig: LadderRig, toolCallId: string): Promise<void> {
  const input = { command: "sleep 100000", pty: true };
  rig.watchdog.callRequested({ toolName: "bash", toolCallId, input });
  rig.watchdog.executionStarted({ toolName: "bash", toolCallId, input });
  rig.tick(10 * MINUTE + SECOND);
  await rig.watchdog.sweep();
  rig.tick(2 * MINUTE);
  await rig.watchdog.sweep();
  rig.tick(2 * MINUTE);
  await rig.watchdog.sweep();
}

/** The call the host closed, which is a stuck call this session is done with. */
function closed(rig: LadderRig, toolCallId: string): void {
  rig.watchdog.executionEnded({ toolCallId });
}

test("the process is ended at most twice, and a third stuck call in one session ends nothing", async () => {
  const directory = stateDir();
  const log = warnings();
  const exits = createStallExits({ stateDir: directory, session: WORKER, now: () => START, log });
  const rig = ladder(exits);

  await stall(rig, "toolu_1");
  expect(rig.exitCodes).toEqual([STALL_EXIT_CODE]);
  expect(exits.read()).toBe(1);
  closed(rig, "toolu_1");

  await stall(rig, "toolu_2");
  expect(rig.exitCodes).toEqual([STALL_EXIT_CODE, STALL_EXIT_CODE]);
  expect(exits.read()).toBe(STALL_EXIT_CAP);
  closed(rig, "toolu_2");

  // The cap: the worker ends nothing further and its owner is told why, so the daemon's own
  // death budget keeps the claim relaunchable instead of failing it.
  await stall(rig, "toolu_3");
  expect(rig.exitCodes).toEqual([STALL_EXIT_CODE, STALL_EXIT_CODE]);
  expect(exits.read()).toBe(STALL_EXIT_CAP);
  expect(rig.stopped).toHaveLength(1);
  expect(rig.stopped[0]?.call.toolCallId).toBe("toolu_3");
  expect(rig.stopped[0]?.reason).toContain(`cap of ${STALL_EXIT_CAP}`);
  expect(rig.steps.filter((step) => step.step === "exit-process")).toHaveLength(3);
  expect(log.lines).toEqual([]);
});

test("the count survives the relaunch the exit earns, in the same pod's state directory", async () => {
  const directory = stateDir();
  const deps = { stateDir: directory, session: WORKER, now: () => START, log: warnings() };

  // The worker that stalls, ends itself, and is relaunched by the daemon on the same claim.
  const first = ladder(createStallExits(deps));
  await stall(first, "toolu_1");
  expect(first.exitCodes).toEqual([STALL_EXIT_CODE]);

  // A new process of the same pod, reading the count its predecessor left: its own exit is the
  // second, which is the cap.
  const second = ladder(createStallExits(deps));
  await stall(second, "toolu_2");
  expect(second.exitCodes).toEqual([STALL_EXIT_CODE]);
  expect(createStallExits(deps).read()).toBe(STALL_EXIT_CAP);

  // The third launch ends nothing at all and tells its owner instead.
  const third = ladder(createStallExits(deps));
  await stall(third, "toolu_3");
  expect(third.exitCodes).toEqual([]);
  expect(third.stopped[0]?.reason).toContain(`cap of ${STALL_EXIT_CAP}`);
  expect(createStallExits(deps).read()).toBe(STALL_EXIT_CAP);
});

test("a counter file that cannot be read ends no process and tells the owner", async () => {
  const directory = stateDir();
  mkdirSync(stallExitsFile(directory, WORKER));
  const rig = ladder(
    createStallExits({ stateDir: directory, session: WORKER, now: () => START, log: warnings() })
  );

  await stall(rig, "toolu_1");
  expect(rig.exitCodes).toEqual([]);
  expect(rig.stopped).toHaveLength(1);
  expect(rig.stopped[0]?.reason).toContain("the stall exit count could not be read");
  expect(rig.steps.map((step) => step.step)).toEqual([
    "end-processes",
    "abort-turn",
    "exit-process",
  ]);
});

test("a counter file that cannot be written ends no process and tells the owner", async () => {
  const directory = stateDir();
  symlinkSync(path.join(directory, "gone", "count.json"), stallExitsFile(directory, WORKER));
  const rig = ladder(
    createStallExits({ stateDir: directory, session: WORKER, now: () => START, log: warnings() })
  );

  await stall(rig, "toolu_1");
  expect(rig.exitCodes).toEqual([]);
  expect(rig.stopped).toHaveLength(1);
  expect(rig.stopped[0]?.reason).toContain("the stall exit count could not be written");
});

test("a run whose evidence and owner's notice were not written does not reach the exit", async () => {
  const directory = stateDir();
  const log = warnings();
  const exits = createStallExits({ stateDir: directory, session: WORKER, now: () => START, log });
  const steps: StallStepRecord[] = [];

  // The real teller, with a transcript that refuses the record and no owner to publish to: the
  // controller's own session is the owner, so nothing is told, and `appendEntry` throwing leaves
  // the evidence unwritten.
  const teller = createStallTeller({
    now: () => START,
    session: () => ({ kind: "controller" }),
    claim: () => undefined,
    project: undefined,
    readState: async () => {
      throw new Error("the daemon is not asked for the controller's own notice");
    },
    publisher: () => undefined,
    appendEntry: () => {
      throw new Error("this session's transcript is closed");
    },
    log: { debug: () => {}, warn: () => {} },
  });
  const rig = ladder(exits, {
    tellOverdue: teller.tellOverdue,
    recordStep: (record) => {
      steps.push(record);
      return teller.recordStep(record);
    },
    tellStopped: teller.tellStopped,
  });

  await stall(rig, "toolu_1");
  // The two cheap steps ran; the one that would have ended the process did not, and nothing was
  // counted against this worker's budget.
  expect(steps.map((step) => step.step)).toEqual(["end-processes", "abort-turn", "exit-process"]);
  expect(steps[2]?.outcome).toContain("the evidence for this call was not written");
  expect(rig.exitCodes).toEqual([]);
  expect(exits.read()).toBe(0);
});

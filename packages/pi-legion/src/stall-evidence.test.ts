import { afterEach, expect, test } from "bun:test";
import { mkdtempSync, rmSync } from "node:fs";
import { tmpdir } from "node:os";
import path from "node:path";
import type { LegionState } from "@legion/contracts/legion-api";
import type { PublishInput } from "@legion/envoy-client/transport";
import type { ClaimCapability } from "@legion/pi-shared/legion-phase-ops";
import type { EnvoyPublish } from "@legion/pi-shared/phase-reminder";
import { DEFAULT_STALL_LIMITS, defaultCallBoundMs } from "@legion/pi-shared/stall-limits";
import {
  createStallWatchdog,
  type StallCall,
  type StallEvidence,
  type StallWatchdog,
  type StallWatchdogTeller,
} from "@legion/pi-shared/stall-watchdog";
// The import above is this file's merge-base check: on the merge base there is no
// `./stall-evidence`, so every test here fails to resolve rather than passing vacuously.
import type { LegionSessionKind } from "./classify";
import {
  boundedText,
  createStallTeller,
  readStallProcesses,
  STALL_DIGEST_LIMIT,
  STALL_READ_ENVIRONMENT,
  STALL_WATCH_ENTRY,
  type StallEvidenceRecord,
  type StallWatchRecord,
  stallArgumentsDigest,
  stallEvidenceRecord,
  stallNoticeTopic,
  stallProcessTree,
} from "./stall-evidence";

const SECOND = 1_000;
const MINUTE = 60 * SECOND;
const START = 1_793_000_000_000;

// ---------------------------------------------------------------------------------------------
// Real processes, read from this pod's own /proc.
// ---------------------------------------------------------------------------------------------

const spawned: Bun.Subprocess[] = [];

afterEach(() => {
  for (const child of spawned.splice(0)) child.kill("SIGKILL");
});

/** One real child of this process, with the environment the case needs, left running until the
 * case ends. `sh -c` is what makes a process of its own: a command Oh My Pi runs in its own shell
 * has none (measured). */
async function child(environment: Record<string, string>): Promise<number> {
  // `exec`, so the process the case reads is the one `kill` ends: a shell that forks `sleep`
  // leaves it running when the shell is killed, reparented to pid 1 (measured).
  const running = Bun.spawn(["sh", "-c", "exec sleep 300"], {
    env: { ...process.env, ...environment },
    stdio: ["ignore", "ignore", "ignore"],
  });
  spawned.push(running);
  // The pid is live the moment `spawn` returns, but the kernel carries the process and its
  // environment a moment later. What is awaited is that reading, not a guessed delay: this case
  // is about a real process tree, so there is no clock to fake.
  await until(async () => {
    const scan = await readStallProcesses();
    if (!scan.read) return false;
    const found = scan.processes.find((candidate) => candidate.pid === running.pid);
    return (
      found !== undefined &&
      found.environment.PI_TOOL_CALL_ID === environment.PI_TOOL_CALL_ID &&
      found.command.includes("sleep 300")
    );
  });
  return running.pid;
}

/** Waits for one reading of the machine to come true, up to ten seconds. */
async function until(ready: () => Promise<boolean>): Promise<void> {
  for (let attempt = 0; attempt < 1_000; attempt += 1) {
    if (await ready()) return;
    await Bun.sleep(10);
  }
  throw new Error("the machine never reached the state the case needs");
}

test("the tree names every process of the agent, read from this pod's own /proc", async () => {
  const pid = await child({
    PI_TOOL_CALL_ID: "toolu_stuck",
    // A credential of the kind an environment routinely holds: the reader keeps two names and
    // this is not one of them, so nothing downstream can write it anywhere.
    LEGION_CLAIM_SECRET: "a-secret-nothing-may-record",
  });

  const scan = await readStallProcesses();
  if (!scan.read) throw new Error(`this pod's /proc could not be read: ${scan.reason}`);
  const agent = scan.processes[0];
  expect(agent?.pid).toBe(process.pid);
  // Measurement 1: the agent's own environment is the one `/proc` will not hand it here, so the
  // reader reads other pids only.
  expect(agent?.environment).toEqual({});

  const found = scan.processes.find((candidate) => candidate.pid === pid);
  expect(found).toBeDefined();
  expect(found?.ppid).toBe(process.pid);
  expect(found?.command).toContain("sleep 300");
  // The identifier, the start time, the state and the processor time, as the kernel reports them.
  expect(found?.startedAt).toBeGreaterThan(Date.now() - 60 * SECOND);
  expect(found?.startedAt).toBeLessThanOrEqual(Date.now());
  expect(found?.state).toMatch(/^[A-Za-z]$/);
  expect(found?.cpuMs).toBeGreaterThanOrEqual(0);
  // The call its environment names, which is exactly the frame's `toolCallId` (measured).
  expect(found?.toolCallId).toBe("toolu_stuck");
  // And nothing else of that environment is kept, whatever else the process inherited.
  expect(
    Object.keys(found?.environment ?? {}).filter(
      (name) => !STALL_READ_ENVIRONMENT.some((wanted) => wanted === name)
    )
  ).toEqual([]);

  // What the evidence carries: the same processes, with no environment and no parent.
  const tree = stallProcessTree(scan);
  if (!tree.read) throw new Error("the tree was read and then reported unread");
  expect(JSON.stringify(tree)).not.toContain("a-secret-nothing-may-record");
  expect(tree.processes.find((candidate) => candidate.pid === pid)).toEqual({
    pid,
    command: found?.command as string,
    startedAt: found?.startedAt as number,
    state: found?.state as string,
    cpuMs: found?.cpuMs as number,
    toolCallId: "toolu_stuck",
  });
});

test("a machine whose /proc cannot be read records that, never an empty tree", async () => {
  const absent = path.join(mkdtempSync(path.join(tmpdir(), "stall-proc-")), "proc");
  try {
    const scan = await readStallProcesses({ procRoot: absent });
    expect(scan.read).toBe(false);
    if (scan.read) throw new Error("a /proc that is not there was read");
    expect(scan.reason).toContain(absent);
    // And the evidence says the same thing, rather than carrying no process at all.
    expect(stallProcessTree(scan)).toEqual({ read: false, reason: scan.reason });
    const record = stallEvidenceRecord(evidenceOf({ tree: stallProcessTree(scan) }), START, {
      session: { kind: "phase-worker", role: "tester", tree: "INFRA-1000", issue: "INFRA-1274" },
      claim: claimOf(),
    });
    expect(record.tree).toEqual({ read: false, reason: scan.reason });
  } finally {
    rmSync(path.dirname(absent), { recursive: true, force: true });
  }
});

// ---------------------------------------------------------------------------------------------
// The record: the tool, the digest, the start, the age, the tree.
// ---------------------------------------------------------------------------------------------

/** One overdue call, with whatever the case needs changed. */
function callOf(over: Partial<StallCall> = {}): StallCall {
  return {
    toolCallId: "toolu_stuck",
    toolName: "bash",
    input: { command: "sleep 100000", pty: true },
    startedAt: START,
    deadlineMs: undefined,
    step: 0,
    steppedAt: undefined,
    told: undefined,
    ...over,
  };
}

function evidenceOf(over: Partial<StallEvidence> = {}): StallEvidence {
  return {
    call: callOf(),
    overdue: {
      rule: { kind: "bound", boundMs: 60 * MINUTE },
      openMs: 65 * MINUTE,
      pastMs: 5 * MINUTE,
    },
    tree: { read: true, processes: [] },
    ...over,
  };
}

function claimOf(over: Partial<ClaimCapability> = {}): ClaimCapability {
  return {
    sessionID: "ses_worker",
    claimToken: "claim-token",
    generation: 3,
    tree: "INFRA-1000",
    issue: "INFRA-1274",
    role: "implementer",
    secret: "claim-secret",
    ...over,
  };
}

test("the record names the tool, the digest, the start, the age and the real tree", async () => {
  const pid = await child({ PI_TOOL_CALL_ID: "toolu_stuck" });
  const tree = stallProcessTree(await readStallProcesses());
  const at = START + 65 * MINUTE;
  const record = stallEvidenceRecord(evidenceOf({ tree }), at, {
    session: { kind: "phase-worker", role: "implementer", tree: "INFRA-1000", issue: "INFRA-1274" },
    claim: claimOf(),
  });

  expect(record.kind).toBe("evidence");
  expect(record.at).toBe(new Date(at).toISOString());
  expect(record.issue).toBe("INFRA-1274");
  expect(record.role).toBe("implementer");
  expect(record.toolCallId).toBe("toolu_stuck");
  expect(record.tool).toBe("bash");
  expect(record.arguments).toBe('command="sleep 100000" pty=true');
  expect(record.startedAt).toBe(new Date(START).toISOString());
  expect(record.openMs).toBe(65 * MINUTE);
  expect(record.allowedMs).toBe(60 * MINUTE);
  expect(record.rule).toEqual({ kind: "bound", boundMs: 60 * MINUTE });

  if (!record.tree.read) throw new Error(`the tree was not read: ${record.tree.reason}`);
  const process = record.tree.processes.find((candidate) => candidate.pid === pid);
  expect(process?.command).toContain("sleep 300");
  expect(process?.startedAt).toMatch(/^\d{4}-\d{2}-\d{2}T/);
  expect(process?.state).toMatch(/^[A-Za-z]$/);
  expect(typeof process?.cpuMs).toBe("number");
  expect(process?.toolCallId).toBe("toolu_stuck");
});

test("the record's allowance is the call's own deadline plus the grace where it promised one", () => {
  const record = stallEvidenceRecord(
    evidenceOf({
      overdue: {
        rule: { kind: "deadline", deadlineMs: 5 * MINUTE, graceMs: 5 * MINUTE },
        openMs: 11 * MINUTE,
        pastMs: MINUTE,
      },
    }),
    START,
    { session: { kind: "controller" }, claim: undefined }
  );
  expect(record.allowedMs).toBe(10 * MINUTE);
  expect(record.session).toBe("controller");
  expect(record.issue).toBeUndefined();
  expect(record.role).toBeUndefined();
});

test("an argument far longer than the bound is cut to it rather than written whole", () => {
  const tail = "end-of-the-very-long-argument";
  const digest = stallArgumentsDigest({ command: `${"x".repeat(5_000)} ${tail}`, timeout: 0 });
  expect(digest.length).toBeLessThanOrEqual(STALL_DIGEST_LIMIT);
  expect(digest).not.toContain(tail);
  expect(digest).toContain("characters in all");
  expect(digest.startsWith('command="xxxx')).toBe(true);

  // The bound is a bound, whatever it is set to, and the marker is counted against it.
  expect(boundedText("x".repeat(100), 40).length).toBe(40);
  expect(boundedText("short", 40)).toBe("short");
  // A command's many lines read as one.
  expect(stallArgumentsDigest({ command: "one\n  two\n" })).toBe('command="one\\n two\\n"');
});

// ---------------------------------------------------------------------------------------------
// The telling: who hears, and how often.
// ---------------------------------------------------------------------------------------------

interface TellerRig {
  readonly teller: StallWatchdogTeller;
  readonly entries: StallWatchRecord[];
  readonly published: PublishInput[];
  readonly warnings: { readonly message: string; readonly context?: Record<string, unknown> }[];
  /** What the listener answers; a string makes the publish throw that message. */
  publishAnswer: { kept?: boolean } | string;
  /** Whether Envoy is on for this session. */
  envoy: boolean;
  /** Whether `pi.appendEntry` takes the record. */
  transcriptWritable: boolean;
  /** The daemon's answer to the record check; a string makes the read throw. */
  state: LegionState | string;
}

function tellerRig(
  options: {
    session?: LegionSessionKind;
    claim?: ClaimCapability | undefined;
    project?: string;
    now?: () => number;
  } = {}
): TellerRig {
  const state: Omit<TellerRig, "teller"> = {
    entries: [],
    published: [],
    warnings: [],
    publishAnswer: {},
    envoy: true,
    transcriptWritable: true,
    state: daemonState("working"),
  };
  const publish: EnvoyPublish = async (input) => {
    state.published.push(input);
    if (typeof state.publishAnswer === "string") throw new Error(state.publishAnswer);
    return state.publishAnswer;
  };
  const teller = createStallTeller({
    now: options.now ?? (() => START),
    session: () =>
      options.session ?? {
        kind: "phase-worker",
        role: "implementer",
        tree: "INFRA-1000",
        issue: "INFRA-1274",
      },
    claim: () => ("claim" in options ? options.claim : claimOf()),
    project: options.project ?? "legion",
    readState: async () => {
      if (typeof state.state === "string") throw new Error(state.state);
      return state.state;
    },
    publisher: () => (state.envoy ? publish : undefined),
    appendEntry: (customType, data) => {
      expect(customType).toBe(STALL_WATCH_ENTRY);
      if (!state.transcriptWritable) throw new Error("the transcript is not writable");
      state.entries.push(data as StallWatchRecord);
    },
    log: {
      debug: () => {},
      warn: (message, context) => state.warnings.push({ message, ...(context ? { context } : {}) }),
    },
    recordBoundMs: 200,
  });
  return Object.assign(state, { teller });
}

/** The daemon's state with this issue's own architect claim in `state`. */
function daemonState(architect: string | undefined): LegionState {
  return {
    issues: {
      "INFRA-1274": {
        ...(architect === undefined
          ? {}
          : { architect: { session: "ses_architect", state: architect, pluginContract: 56 } }),
      },
    },
  } as unknown as LegionState;
}

test("a phase worker's session tells the issue's own live architect, with the evidence", async () => {
  const rig = tellerRig();
  const told = await rig.teller.tellOverdue(evidenceOf());

  expect(told).toEqual({ evidenceWritten: true, ownerTold: true });
  // The record goes into the session's transcript.
  const written = rig.entries[0] as StallEvidenceRecord;
  expect(written.kind).toBe("evidence");
  expect(written.tool).toBe("bash");
  expect(written.arguments).toBe('command="sleep 100000" pty=true');
  // And into the message: the same record, as the payload the listener takes.
  expect(rig.published).toHaveLength(1);
  const notice = rig.published[0] as PublishInput;
  expect(notice.topic).toBe("notifications.role.legion-legion-infra-1274-architect");
  expect(notice.message).toContain("The implementer of INFRA-1274 has a `bash` call");
  expect(notice.message).toContain('command="sleep 100000" pty=true');
  expect(JSON.parse(notice.payload as string)).toEqual(written);
  expect(notice.idempotencyKey).toBe("legion-stall:toolu_stuck");
  expect(notice.keep).toBe(false);
  expect("sourceSessionID" in notice && notice.sourceSessionID).toBe("ses_worker");
});

test("a phase worker falls back to the tree's architect, as the reminder does", async () => {
  for (const architect of ["retired", "failed", undefined]) {
    const rig = tellerRig();
    rig.state = daemonState(architect);
    await rig.teller.tellOverdue(evidenceOf());
    expect(rig.published[0]?.topic).toBe("notifications.role.legion-legion-infra-1000-architect");
  }
  // A daemon that does not answer inside the bound is the same: the tree's architect is told.
  const silent = tellerRig();
  silent.state = "the daemon is unreachable";
  await silent.teller.tellOverdue(evidenceOf());
  expect(silent.published[0]?.topic).toBe("notifications.role.legion-legion-infra-1000-architect");
});

test("a root architect's own session tells the controller role", async () => {
  const rig = tellerRig({
    session: { kind: "root-architect", tree: "INFRA-1000" },
    claim: claimOf({ issue: "INFRA-1000", role: "architect", sessionID: "ses_root" }),
  });
  await rig.teller.tellOverdue(evidenceOf());
  expect(rig.published).toHaveLength(1);
  expect(rig.published[0]?.topic).toBe("notifications.role.legion-legion-controller");
  expect(rig.published[0]?.message).toContain("The architect of INFRA-1000");
});

test("the controller's own session publishes nothing and writes its evidence instead", async () => {
  const rig = tellerRig({ session: { kind: "controller" }, claim: undefined });
  const told = await rig.teller.tellOverdue(evidenceOf());

  expect(rig.published).toEqual([]);
  expect(told).toEqual({ evidenceWritten: true, ownerTold: false });
  expect((rig.entries[0] as StallEvidenceRecord).session).toBe("controller");
  expect(rig.warnings.map((line) => line.message)).toContain(
    "legion: a tool call of this session has not returned"
  );
  expect(rig.warnings.map((line) => line.message)).toContain(
    "legion: nobody was told about this session's stuck tool call"
  );
});

test("a session never publishes to the role its own session holds", async () => {
  // A sub-architect is a phase worker whose own claim is its issue's live architect.
  const rig = tellerRig({
    session: { kind: "phase-worker", role: "architect", tree: "INFRA-1000", issue: "INFRA-1300" },
    claim: claimOf({ issue: "INFRA-1300", role: "architect", sessionID: "ses_sub" }),
  });
  rig.state = {
    issues: {
      "INFRA-1300": { architect: { session: "ses_sub", state: "working", pluginContract: 56 } },
    },
  } as unknown as LegionState;
  await rig.teller.tellOverdue(evidenceOf());
  expect(rig.published[0]?.topic).toBe("notifications.role.legion-legion-infra-1000-architect");

  // And the controller's own session, which holds the only role its notice could reach.
  expect(
    stallNoticeTopic({
      session: { kind: "controller" },
      claim: undefined,
      project: "legion",
      liveArchitect: true,
    })
  ).toEqual({
    kind: "none",
    why: "the controller's own session is the owner, so its evidence is its transcript and its log",
  });
});

test("a refused publish and a session with Envoy off leave a line in the log", async () => {
  const refused = tellerRig();
  refused.publishAnswer = 'listener returned 404 Not Found: {"reason":"unclaimed"}';
  expect(await refused.teller.tellOverdue(evidenceOf())).toEqual({
    evidenceWritten: true,
    ownerTold: false,
  });
  expect(refused.warnings.map((line) => line.message)).toContain(
    "legion: the owner was not told about this session's stuck tool call"
  );

  // A listener that kept the notice has told no architect, so nobody was told.
  const kept = tellerRig();
  kept.publishAnswer = { kept: true };
  expect((await kept.teller.tellOverdue(evidenceOf())).ownerTold).toBe(false);

  const off = tellerRig();
  off.envoy = false;
  expect(await off.teller.tellOverdue(evidenceOf())).toEqual({
    evidenceWritten: true,
    ownerTold: false,
  });
  expect(off.published).toEqual([]);
  expect(off.warnings.map((line) => line.message)).toContain(
    "legion: Envoy is off for this session, so its owner was not told"
  );

  // A transcript the host refuses leaves the evidence unwritten, which is what stops the ladder's
  // last step; the owner is still told.
  const unwritable = tellerRig();
  unwritable.transcriptWritable = false;
  expect(await unwritable.teller.tellOverdue(evidenceOf())).toEqual({
    evidenceWritten: false,
    ownerTold: true,
  });

  // A claim whose issue key no role token can be built from is one owner not told, and the
  // evidence is written all the same.
  const unbuildable = tellerRig({ claim: claimOf({ issue: "not-an-issue-key" }) });
  expect(await unbuildable.teller.tellOverdue(evidenceOf())).toEqual({
    evidenceWritten: true,
    ownerTold: false,
  });
  expect(unbuildable.published).toEqual([]);
});

test("each step and each stop is recorded, and a stop is told once", async () => {
  const rig = tellerRig();
  await rig.teller.recordStep({
    call: callOf(),
    step: "end-processes",
    openMs: 65 * MINUTE,
    outcome: "ended 1 process of the call",
  });
  expect(rig.entries[0]).toEqual({
    kind: "step",
    at: new Date(START).toISOString(),
    toolCallId: "toolu_stuck",
    tool: "bash",
    step: "end-processes",
    openMs: 65 * MINUTE,
    outcome: "ended 1 process of the call",
  });

  await rig.teller.tellStopped(callOf(), "the exit cap was reached");
  await rig.teller.tellStopped(callOf(), "the exit cap was reached");
  expect(rig.entries.filter((entry) => entry.kind === "stopped")).toHaveLength(2);
  // The owner hears why the ladder stopped, and hears it once.
  expect(rig.published).toHaveLength(1);
  expect(rig.published[0]?.message).toContain("cannot recover from on its own");
  expect(rig.published[0]?.idempotencyKey).toBe("legion-stall-stopped:toolu_stuck");
});

// ---------------------------------------------------------------------------------------------
// Driven by the detector itself: one message per stuck call, and none for a call that ends.
// ---------------------------------------------------------------------------------------------

function watchdogWith(teller: StallWatchdogTeller, clock: () => number): StallWatchdog {
  return createStallWatchdog({
    now: clock,
    limits: { ...DEFAULT_STALL_LIMITS, callBoundMs: defaultCallBoundMs("tester") },
    isIdle: () => false,
    processes: { tree: () => ({ read: true, processes: [] }), endCall: () => 0 },
    teller,
    aborter: { abortTurn: () => {}, sendMessage: () => {} },
    exits: { read: () => 0, write: () => true },
    exitProcess: () => {},
    log: { debug: () => {}, warn: () => {} },
  });
}

test("a second sweep of the same still-open call sends no second message", async () => {
  let clock = START;
  const rig = tellerRig({ now: () => clock });
  const watchdog = watchdogWith(rig.teller, () => clock);
  watchdog.callRequested({ toolName: "eval", toolCallId: "toolu_stuck", input: { code: "1" } });
  watchdog.executionStarted({ toolName: "eval", toolCallId: "toolu_stuck", input: { code: "1" } });

  clock += 61 * MINUTE;
  await watchdog.sweep();
  expect(rig.published).toHaveLength(1);

  // The call is still open, and the ladder carries it further without telling anyone again.
  clock += 3 * MINUTE;
  await watchdog.sweep();
  clock += 3 * MINUTE;
  await watchdog.sweep();
  expect(rig.published).toHaveLength(1);
  expect(rig.entries.filter((entry) => entry.kind === "evidence")).toHaveLength(1);
  expect(rig.entries.filter((entry) => entry.kind === "step").length).toBeGreaterThan(1);

  // And a teller told twice about one call repeats nothing, whatever reaches it.
  const again = await rig.teller.tellOverdue(evidenceOf({ call: callOf({ toolName: "eval" }) }));
  expect(again).toEqual({ evidenceWritten: true, ownerTold: true });
  expect(rig.published).toHaveLength(1);
});

test("a call that ends before the bound sends nothing at all", async () => {
  let clock = START;
  const rig = tellerRig({ now: () => clock });
  const watchdog = watchdogWith(rig.teller, () => clock);
  watchdog.callRequested({ toolName: "eval", toolCallId: "toolu_quick", input: {} });
  watchdog.executionStarted({ toolName: "eval", toolCallId: "toolu_quick", input: {} });
  clock += 59 * MINUTE;
  await watchdog.sweep();
  watchdog.executionEnded({ toolCallId: "toolu_quick" });
  clock += 10 * MINUTE;
  await watchdog.sweep();

  expect(rig.published).toEqual([]);
  expect(rig.entries).toEqual([]);
});

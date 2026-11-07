import { afterEach, expect, test } from "bun:test";
import { chmod, mkdir, mkdtemp, readdir, readFile, rm, writeFile } from "node:fs/promises";
import * as os from "node:os";
import * as path from "node:path";
import { STALL_EXIT_CAP, STALL_EXIT_CODE } from "@legion/pi-shared/stall-watchdog";
import {
  type Block,
  type Cleanup,
  call,
  messageStream,
  ompRoot,
  type Request,
  refusingNats,
  serveStandin,
  spawnRpc,
  writeStandinProfile,
} from "@legion/pi-shared/test/omp-harness";
import { STALL_RESTART_MESSAGE } from "../src/stall-abort";
import { STALL_WATCH_ENTRY, type StallWatchRecord } from "../src/stall-evidence";
import { stallExitsFile } from "../src/stall-exits";

// The stall watch's ladder on the real Oh My Pi (INFRA-1274's `k6`): a worker whose tool call
// stops, carried through the three steps of `@legion/pi-shared/stall-watchdog` by this package's
// four collaborators, against the binary that ships. Only the real binary can say any of this: the
// clock the watch judges a call by starts at the host's own `tool_execution_start`, the processes
// the first step ends are the ones this host really started, the second step is the host's
// `ctx.abort()` and the turn it does or does not end, and the last step is this process exiting
// with the status the daemon reads as a death.
//
// LEGION_TEST_OMP names the binary, as in legion-phase-stall-omp.test.ts: a run without one skips,
// except on GitHub Actions, where a skip would hide the only run of the check on the host that
// ships it (GITHUB_ACTIONS, not CI: agent harnesses on the devbox export CI=true).
//
// **What was measured here on 2026-11-07, against `/opt/omp/bin/omp` (`omp/18.16.0`) in
// `--mode rpc`**, and what each case below is built on:
//
//  1. **An `eval` cell with an explicit long deadline holds the turn open and writes no end
//     frame**: no `tool_execution_end`, no `turn_end`, no `agent_end`, until something ends it.
//     Its deadline is one the host may pause (`STALL_PAUSED_DEADLINE_TOOLS`), so the watch judges
//     it by the role's bound alone, which is the number these cases set low.
//  2. **`tool_execution_start` reaches the plugin about a second after the cell has begun.** The
//     kernel, and a child the cell starts at once, are already running when the watch starts the
//     call's clock, so the "started after the call" rule (`src/stall-recovery.ts`, rule 2) cannot
//     see them. Each case that wants that rule has its cell wait before it forks; the case that
//     wants the runner rule (the last one here) runs a cell with no child at all.
//  3. **`ctx.abort()` ends every wedged call this file could build, and resolves only once that
//     call has ended.** Five shapes were tried against the pin: an `eval` cell looping in
//     `time.sleep`; one ignoring SIGINT; one ignoring SIGINT, SIGTERM and SIGHUP, which the host
//     answered by shutting the kernel down; one blocked in a host `wait()` on a subagent whose
//     model request never answered; and a `pty: true` shell command trapping every signal. Each
//     came back within about five seconds of the abort, with the turn ended — and a turn that
//     ends closes the call's record, so the ladder's last step, which runs *only* where the abort
//     did not end the turn, has no tool call on this binary that can reach it. The two cases that
//     prove that last step therefore take `abort` off the session context the host hands its
//     extensions, with a one-handler extension written into the case's own scratch root: that is
//     exactly the host `src/stall-abort.ts` documents ("A host with no `abort()` … throws from
//     here"). Nothing of the plugin is replaced, and everything else in those cases is the real
//     thing — the real worker exits, the real counter file is written and read back by the
//     relaunch, and the real notice reaches the listener.
const omp = process.env.LEGION_TEST_OMP;
const onActions = process.env.GITHUB_ACTIONS === "true";
const skip = omp === undefined && !onActions;

/** The binary the case runs against; throws on GitHub Actions without it. */
function setup(): string {
  if (omp === undefined) throw new Error("LEGION_TEST_OMP is unset on GitHub Actions");
  return omp;
}

const PROJECT = "watch";
const ROLE = "implementer";
const TREE = "WATCH-1";
const ISSUE = "WATCH-2";

/** The daemon's assignment, sent as the RPC prompt. Every turn of the pane's own conversation
 * carries it, which is what tells a turn from a side request. */
const ASSIGNMENT = `Implement ${ISSUE}. WATCH-ROLE-ASSIGNMENT.`;

/** Who a phase worker's stuck call is told about: the tree's architect, on the role topic
 * `roleToken` builds (`packages/contracts/src/legion-roles.ts`). */
const ARCHITECT_TOPIC = `notifications.role.legion-${PROJECT}-${TREE.toLowerCase()}-architect`;

/** The bound for a call no deadline judges (`LEGION_STALL_CALL_BOUND_MS`): six seconds, so a
 * wedged cell is overdue one sweep after it starts rather than four hours later. */
const BOUND_MS = 6_000;

/** The gap between the ladder's steps (`LEGION_STALL_STEP_MS`): four seconds, so each step lands
 * on the sweep after the one before it. */
const STEP_MS = 4_000;

/** How long a case waits for something the pane owes it before failing: the pane's boot and the
 * three steps of the ladder, each a 5-second sweep apart. */
const DEADLINE_MS = 120_000;

/** How long a case watches for something that must not happen: the bound, two of the plugin's
 * 5-second sweeps, and a margin. */
const QUIET_MS = BOUND_MS + 12_000;

const cleanup: Cleanup = [];
afterEach(async () => {
  for (const step of cleanup.splice(0).reverse()) await step();
});

/** A Messages request's whole conversation, as one string to find texts in. */
function conversation(request: Request): string {
  return JSON.stringify(request.body.messages ?? null);
}

/** A Messages request's last message, which is what started the turn it answers. */
function lastMessage(request: Request): string {
  const messages = Array.isArray(request.body.messages) ? request.body.messages : [];
  return JSON.stringify(messages.at(-1) ?? null);
}

/**
 * Whether a process is still running, asked of `/proc` rather than read from a log line. A
 * process the kernel holds as a zombie has ended and is waiting for its parent to reap it, which
 * a signal of 0 still answers for: a cell that was killed while looping never reaps the child the
 * first step ended, so the state letter is what says the child is gone.
 */
async function runs(pid: number): Promise<boolean> {
  if (!Number.isInteger(pid) || pid <= 0) throw new Error(`${pid} is no process to ask about`);
  let stat: string;
  try {
    stat = await readFile(`/proc/${pid}/stat`, "utf8");
  } catch {
    return false;
  }
  // `/proc/<pid>/stat` holds the executable's name in parentheses, which may itself hold spaces
  // and parentheses, and the state letter is the field after the last of them.
  return (
    stat
      .slice(stat.lastIndexOf(")") + 1)
      .trim()
      .charAt(0) !== "Z"
  );
}

/** A directory of this case's own: the claim's state directory, or where a cell writes the pid of
 * the child it started. */
async function scratchDir(name: string): Promise<string> {
  const directory = await mkdtemp(path.join(os.tmpdir(), `legion-watch-${name}-`));
  cleanup.push(() => rm(directory, { recursive: true, force: true }));
  return directory;
}

/** The counter file one claim counts its own stall exits in, under `stateDir`. */
function exitsFile(stateDir: string): string {
  return stallExitsFile(stateDir, { kind: "phase-worker", role: ROLE, tree: TREE, issue: ISSUE });
}

/**
 * The cell a case wedges its turn with. It waits `wait` seconds before it starts its child, so the
 * host's `tool_execution_start` has reached the plugin and the child is a process rule 2 can see
 * (measurement 2); with `goOn` it keeps running after that child has died, which is what carries
 * the ladder to its second step, and without it the cell is waiting on the child and returns the
 * moment the first step ends it.
 */
function wedgeCell(pidFile: string, options: { readonly wait: number; readonly goOn: boolean }) {
  return [
    "import pathlib, subprocess, time",
    `time.sleep(${options.wait})`,
    'p = subprocess.Popen(["/bin/sleep", "3000"])',
    `pathlib.Path(${JSON.stringify(pidFile)}).write_text(str(p.pid))`,
    "print('the child is', p.pid)",
    ...(options.goOn
      ? ["while True:", "    time.sleep(0.5)"]
      : ["p.wait()", "print('the child is gone')"]),
  ].join("\n");
}

/** The daemon's state with the issue past this pane's phase: the reviewer works it now, so the
 * phase-stall follow-up (`src/phase-stall.ts`) has no reminder for this role and every turn a case
 * sees is one it asked for. */
const MOVED_ON = {
  daemon: {
    project: PROJECT.toUpperCase(),
    schemaVersion: 1,
    boots: 1,
    firstBootAt: "2026-10-30T14:03:27Z",
    startedAt: "2026-10-31T09:15:02Z",
  },
  admission: { cap: 1, active: [TREE], waiting: [], blocked: [] },
  issues: {
    [ISSUE]: {
      key: ISSUE,
      generation: 1,
      phase: "reviewing",
      status: "in_progress",
      repo: "acme/widgets",
      defaultBranch: "main",
      workers: {},
    },
  },
  pendingStatusWrites: [],
  heldOutboxRows: [],
  strandedClaims: [],
  capabilities: [],
  repositories: {
    owners: ["acme"],
    mapped: [{ repo: "acme/widgets", default: true }],
    refused: [],
    withoutWebhookApp: [],
    caches: [],
  },
  settings: { max_fix_attempts: 3, worker_boot_timeout_seconds: 120 },
  consumers: [],
};

/**
 * The one-handler extension the last step's cases load beside the plugin. `abort` is not an own
 * property of the session context: it lives on the object every context is made from, so dropping
 * it there leaves every extension of that run — this plugin included — with the host
 * `src/stall-abort.ts` documents, the one whose `ctx.abort()` is not a function. The plugin's own
 * second step still runs; it fails for the host's reason and the ladder goes on to the step that
 * ends the process.
 */
const NO_ABORT_EXTENSION = `export default function noAbort(pi) {
  pi.on("session_start", (_event, context) => {
    const host = Object.getPrototypeOf(context);
    if (host !== null && Object.getOwnPropertyDescriptor(host, "abort") !== undefined) {
      Object.defineProperty(host, "abort", { value: undefined, configurable: true });
    }
  });
}
`;

/** How one pane differs from the others. */
interface PaneOptions {
  /** The scratch root's prefix. */
  readonly name: string;
  /** `LEGION_STATE_DIR`: where this claim's stall exit count lives, shared by its launches. */
  readonly stateDir: string;
  /** Answers turn `index` (1 for the first) of the pane's own conversation. */
  readonly reply: (index: number, request: Request) => readonly Block[];
  /** Loads the extension above, so the host has no `abort()` and the ladder reaches its last
   * step. A pane that loads it may end its own process, which `until` then expects. */
  readonly denyAbort?: boolean;
}

/** One running pane, and what a case may ask of it. */
interface StallPane {
  /** Every request the stand-in served: the gateway's, the daemon's and the listener's. */
  readonly requests: Request[];
  /** The turns of the pane's own conversation, in order. */
  readonly turns: () => Request[];
  /** Every `POST /v1/messages/publish` the pane made: the stuck call's notice, and the notice
   * that the ladder stopped short of a step. */
  readonly publishes: () => Record<string, unknown>[];
  /** This launch's stall-watch records, in the order its transcript holds them. */
  readonly records: () => Promise<StallWatchRecord[]>;
  /** The text of each message the worker sent its own session to start the work again. */
  readonly restarts: () => Promise<string[]>;
  /** The status omp exited with, once it has. */
  readonly exited: Promise<number>;
  /** Whether omp is still running. */
  readonly running: () => boolean;
  /** Polls `probe` until it answers, failing on `what` at the deadline. */
  until<T>(what: string, probe: () => T | undefined | Promise<T | undefined>): Promise<T>;
}

/**
 * Starts one Legion phase-worker pane on the real Oh My Pi: pi-envoy's extension and this
 * package's, the role's `LEGION_*` environment against a stand-in that is the model gateway, the
 * daemon's claim, grant, state and activity routes and the Envoy listener, and the three stall
 * numbers set low. Nothing here takes a NATS delivery, so the Envoy extension is given an address
 * that refuses every connection (`refusingNats`). A side request — the run-end self-check, a
 * routing hint — is answered at once with one word, since none of them is a turn; a turn of the
 * pane's own conversation is answered from `reply`.
 */
async function startPane(options: PaneOptions): Promise<StallPane> {
  const binary = setup();
  const registered: Record<string, unknown> = JSON.parse(
    await readFile(
      path.resolve(import.meta.dir, "../../contracts/fixtures/daemon-api/register.json"),
      "utf8"
    )
  );
  const { root, home, workspace, sessions } = await ompRoot(
    binary,
    `legion-${options.name}-`,
    cleanup
  );

  const turns: Request[] = [];
  let sides = 0;
  let grants = 0;
  const { requests, base } = serveStandin(cleanup, (url, body) => {
    if (url.pathname === "/anthropic/v1/messages") {
      const request: Request = { path: url.pathname, body };
      const own =
        conversation(request).includes("WATCH-ROLE-ASSIGNMENT") &&
        !lastMessage(request).includes("<btw>");
      if (!own) {
        sides += 1;
        return new Response(
          messageStream([{ type: "text", text: "PROCEEDING" }], `side_${sides}`),
          {
            headers: { "content-type": "text/event-stream" },
          }
        );
      }
      turns.push(request);
      return new Response(
        messageStream(options.reply(turns.length, request), `msg_${turns.length}`),
        { headers: { "content-type": "text/event-stream" } }
      );
    }
    if (url.pathname.startsWith("/anthropic/")) return Response.json({ data: [] });
    if (url.pathname === "/legion/v1/claims/register") {
      return Response.json({
        ...registered,
        claimToken: `legion-${PROJECT}-${ISSUE.toLowerCase()}-${ROLE}`,
        tree: TREE,
        issue: ISSUE,
        role: ROLE,
        generation: 1,
        secret: `${PROJECT}-secret`,
      });
    }
    if (url.pathname === "/legion/v1/claims/ready") return new Response(null, { status: 204 });
    if (url.pathname === "/legion/v1/claims/activity") return new Response(null, { status: 204 });
    if (url.pathname === "/legion/v1/state") return Response.json(MOVED_ON);
    if (url.pathname === "/legion/v1/grants") {
      grants += 1;
      return Response.json({
        grantId: `${PROJECT}-grant-${grants}`,
        expiresAt: "2099-01-01T00:00:00Z",
      });
    }
    if (url.pathname.startsWith("/legion/")) {
      return Response.json({ error: `no stand-in route ${url.pathname}` }, { status: 404 });
    }
    if (url.pathname === "/v1/messages/publish") {
      // The listener's answer to a publish it took: an envelope, which the client parses before it
      // answers the worker (`EnvelopeSchema`, packages/contracts/src/envelope.ts).
      const key = String(body.idempotency_key ?? "");
      return Response.json({
        event_id: `evt-${key}`,
        source: "agent",
        source_event_id: key,
        topic: String(body.topic ?? ""),
        dedupe_key: `publish.${key}`,
        issued_at: 1,
        payload_summary: "a worker has a tool call that has not returned",
        trace_id: `trace-${key}`,
      });
    }
    // The Envoy listener: registration, the role claim, and any read answer with an interest.
    return Response.json({
      session_id: typeof body.session_id === "string" ? body.session_id : "",
      machine_id: `${PROJECT}-machine`,
      dir: workspace,
      topics: [],
    });
  });
  await writeStandinProfile(home, base);

  const extensions = [
    path.resolve(import.meta.dir, "../../pi-envoy/extensions/envoy.ts"),
    path.resolve(import.meta.dir, "legion.ts"),
  ];
  if (options.denyAbort === true) {
    const file = path.join(root, "no-abort.ts");
    await writeFile(file, NO_ABORT_EXTENSION);
    extensions.push(file);
  }
  // The `eval` kernel is written under this pane's own TMPDIR, as it is under a pod's, rather than
  // into the machine's shared /tmp.
  const temporary = path.join(root, "tmp");
  await mkdir(temporary, { recursive: true });

  const rpc = spawnRpc(
    binary,
    {
      extensions,
      home,
      workspace,
      sessions,
      env: {
        TMPDIR: temporary,
        ENVOY_URL: base,
        ENVOY_NATS_URL: refusingNats(cleanup),
        LEGION_DAEMON_URL: base,
        LEGION_ROLE: ROLE,
        LEGION_TREE: TREE,
        LEGION_ISSUE: ISSUE,
        LEGION_GENERATION: "1",
        LEGION_PROJECT: PROJECT,
        LEGION_BOOT_TOKEN: `${PROJECT}-boot`,
        LEGION_STATE_DIR: options.stateDir,
        LEGION_WORKSPACE: workspace,
        LEGION_STALL_CALL_GRACE_MS: "1000",
        LEGION_STALL_CALL_BOUND_MS: String(BOUND_MS),
        LEGION_STALL_STEP_MS: String(STEP_MS),
      },
      onFrame: () => undefined,
    },
    cleanup
  );
  // The stream closing is how a case learns the pane ended itself; the rejection is handled here,
  // so a case reads `exited` and `running` rather than racing a promise.
  rpc.closed.catch(() => undefined);
  let running = true;
  void rpc.exited.then(() => {
    running = false;
  });
  rpc.send({ type: "prompt", message: ASSIGNMENT });

  /** This launch's transcript entries of `customType`. The file is appended to as the session
   * runs, so a case reads it while the pane is still running — and after it has ended itself. */
  const entries = async (customType: string): Promise<Record<string, unknown>[]> => {
    const files = (await readdir(sessions, { recursive: true })).filter((file) =>
      file.endsWith(".jsonl")
    );
    if (files.length !== 1) return [];
    return (await readFile(path.join(sessions, files[0] ?? ""), "utf8"))
      .split("\n")
      .filter(Boolean)
      .map((line): unknown => JSON.parse(line))
      .flatMap((entry) =>
        typeof entry === "object" &&
        entry !== null &&
        "customType" in entry &&
        entry.customType === customType
          ? [entry as Record<string, unknown>]
          : []
      );
  };

  const pane: StallPane = {
    requests,
    turns: () => turns,
    publishes: () =>
      requests
        .filter((request) => request.path === "/v1/messages/publish")
        .map((request) => request.body),
    records: async () =>
      (await entries(STALL_WATCH_ENTRY)).map((entry) => entry.data as StallWatchRecord),
    restarts: async () =>
      (await entries(STALL_RESTART_MESSAGE)).map((entry) => String(entry.content ?? "")),
    exited: rpc.exited,
    running: () => running,
    async until(what, probe) {
      const deadline = Date.now() + DEADLINE_MS;
      for (;;) {
        const value = await probe();
        if (value !== undefined) return value;
        if (options.denyAbort !== true && !running) {
          throw new Error(`omp exited while this case waited for ${what}`);
        }
        if (Date.now() > deadline) throw new Error(`timed out waiting for ${what}`);
        // A poll, because what is awaited is a line in a transcript the pane writes and a request
        // it makes, neither of which this process can be woken by.
        await Bun.sleep(100);
      }
    },
  };
  await pane.until("the pane's registration with the Envoy listener", () =>
    requests.find((request) => request.path === "/v1/interests/subscribe")
  );
  return pane;
}

/** The pid the pane's cell wrote to `pidFile`, once it has; the case ends that process at
 * cleanup, so no `sleep` of a case outlives its run. */
async function childOf(pane: StallPane, pidFile: string): Promise<number> {
  const pid = await pane.until("the pid of the child the cell started", async () => {
    try {
      const read = Number(await readFile(pidFile, "utf8"));
      return Number.isInteger(read) && read > 0 ? read : undefined;
    } catch {
      return undefined;
    }
  });
  cleanup.push(async () => {
    try {
      process.kill(pid, "SIGKILL");
    } catch {
      // The step under test ended it, which is what most of these cases are about.
    }
  });
  return pid;
}

/** The records of one kind this launch wrote. */
function ofKind<K extends StallWatchRecord["kind"]>(
  records: readonly StallWatchRecord[],
  kind: K
): Extract<StallWatchRecord, { kind: K }>[] {
  return records.filter(
    (record): record is Extract<StallWatchRecord, { kind: K }> => record.kind === kind
  );
}

/** The step of `kind` this launch recorded, if it has recorded one. */
function stepOf(
  records: readonly StallWatchRecord[],
  step: "end-processes" | "abort-turn" | "exit-process"
) {
  return ofKind(records, "step").find((record) => record.step === step);
}

test.skipIf(skip)(
  "a cell that ends when its child dies: the call is judged from its execution start, the evidence names the child, the owner is told, and the first step ends the child with no abort sent",
  async () => {
    const state = await scratchDir("child-state");
    const pidFile = path.join(await scratchDir("child-pid"), "child.pid");
    const pane = await startPane({
      name: "stall-child",
      stateDir: state,
      reply: (index) =>
        index === 1
          ? [
              call("eval", {
                language: "py",
                code: wedgeCell(pidFile, { wait: 3, goOn: false }),
                timeout: 600,
              }),
            ]
          : [{ type: "text", text: "The call came back." }],
    });
    const child = await childOf(pane, pidFile);

    // The evidence, written before any step ran: the call's own clock, the rule that judged it —
    // the role's bound, since the host may pause an `eval` deadline — and the child in the tree
    // the reader took from `/proc`.
    const evidence = await pane.until("the evidence for the stuck call", async () =>
      ofKind(await pane.records(), "evidence").at(0)
    );
    expect(evidence.tool).toBe("eval");
    expect(evidence.session).toBe("phase-worker");
    expect(evidence.issue).toBe(ISSUE);
    expect(evidence.role).toBe(ROLE);
    expect(evidence.startedAt).not.toBeNull();
    expect(evidence.rule).toEqual({ kind: "bound", boundMs: BOUND_MS });
    expect(evidence.allowedMs).toBe(BOUND_MS);
    expect(evidence.openMs).toBeGreaterThan(BOUND_MS);
    expect(evidence.arguments).toContain('language="py"');
    if (!evidence.tree.read) throw new Error(`the tree was not read: ${evidence.tree.reason}`);
    const seen = evidence.tree.processes.find((process) => process.pid === child);
    expect(seen?.command).toContain("/bin/sleep 3000");

    // The owner's notice: one publish to the tree's architect, the sentence that names the call,
    // and the whole record as its payload.
    const notice = await pane.until("the notice the owner was sent", () => pane.publishes().at(0));
    expect(notice.topic).toBe(ARCHITECT_TOPIC);
    expect(String(notice.message)).toContain(`The ${ROLE} of ${ISSUE}`);
    expect(String(notice.message)).toContain("has a `eval` call that has not returned");
    expect(JSON.parse(String(notice.payload))).toMatchObject({
      kind: "evidence",
      toolCallId: evidence.toolCallId,
    });

    // The first step: the child, and only the child, ended.
    const ended = await pane.until("the first step of the ladder", async () =>
      stepOf(await pane.records(), "end-processes")
    );
    expect(ended.toolCallId).toBe(evidence.toolCallId);
    expect(ended.outcome).toBe("ended 1 process of the call");
    expect(await runs(child)).toBe(false);

    // The cell was waiting on that child, so the call came back on its own: the next turn carries
    // its result, and the ladder never took a second step.
    const after = await pane.until("the turn that answers the call's result", () =>
      pane.turns().find((turn) => conversation(turn).includes("the child is gone"))
    );
    expect(conversation(after)).toContain("the child is gone");
    const records = await pane.records();
    expect(ofKind(records, "step").map((record) => record.step)).toEqual(["end-processes"]);
    expect(ofKind(records, "stopped")).toEqual([]);
    expect(await pane.restarts()).toEqual([]);
    expect(pane.running()).toBe(true);
  },
  DEADLINE_MS + 60_000
);

test.skipIf(skip)(
  "a cell that goes on after its child dies: the abort ends the turn, the session lives, and the worker takes a turn again on the message it sent itself",
  async () => {
    const state = await scratchDir("abort-state");
    const pidFile = path.join(await scratchDir("abort-pid"), "child.pid");
    const pane = await startPane({
      name: "stall-abort",
      stateDir: state,
      reply: (index) =>
        index === 1
          ? [
              call("eval", {
                language: "py",
                code: wedgeCell(pidFile, { wait: 3, goOn: true }),
                timeout: 600,
              }),
            ]
          : [{ type: "text", text: "Noted; I will do something else." }],
    });
    const child = await childOf(pane, pidFile);

    // The same first step as the case above: the evidence, the owner's one notice, the child
    // ended.
    const ended = await pane.until("the first step of the ladder", async () =>
      stepOf(await pane.records(), "end-processes")
    );
    expect(ended.outcome).toBe("ended 1 process of the call");
    expect(await runs(child)).toBe(false);
    expect(pane.publishes().map((publish) => publish.topic)).toEqual([ARCHITECT_TOPIC]);

    // The cell went on without its child, so the second step ran: the host's own abort ended the
    // turn, and the worker put one message in front of its own session.
    const aborted = await pane.until("the second step of the ladder", async () =>
      stepOf(await pane.records(), "abort-turn")
    );
    expect(aborted.outcome).toBe(
      "the turn was ended with the host's abort(); the session was sent the message naming the ended call"
    );
    expect(aborted.openMs).toBeGreaterThanOrEqual(ended.openMs + STEP_MS);

    // The session is the same session, and it took a turn on the message it sent itself.
    const again = await pane.until("the turn the worker's own message started", () =>
      pane.turns().find((turn) => lastMessage(turn).includes("Your turn was ended because"))
    );
    expect(lastMessage(again)).toContain("`eval` call");
    expect(lastMessage(again)).toContain("Decide whether that call is still worth making");
    const restarts = await pane.restarts();
    expect(restarts).toHaveLength(1);
    expect(restarts[0]).toContain("Your turn was ended because its `eval` call");
    expect(pane.running()).toBe(true);

    // The ladder stopped at the abort: the turn it ended closed the call's record, so the step
    // that ends this process never ran, nothing was told that the ladder had stopped, and no
    // exit was counted.
    const records = await pane.records();
    expect(ofKind(records, "step").map((record) => record.step)).toEqual([
      "end-processes",
      "abort-turn",
    ]);
    expect(ofKind(records, "stopped")).toEqual([]);
    await expect(readFile(exitsFile(state), "utf8")).rejects.toThrow();
  },
  DEADLINE_MS + 60_000
);

test.skipIf(skip)(
  "a call the abort does not end: the worker ends its own process, the count survives in the state directory, and the launch that reaches the cap ends nothing and tells its owner",
  async () => {
    const state = await scratchDir("exit-state");
    /** One launch of the same claim on that state directory, whose host has no `abort()`. */
    const launch = async (name: string): Promise<StallPane> => {
      const pidFile = path.join(await scratchDir(`${name}-pid`), "child.pid");
      const pane = await startPane({
        name,
        stateDir: state,
        denyAbort: true,
        reply: (index) =>
          index === 1
            ? [
                call("eval", {
                  language: "py",
                  code: wedgeCell(pidFile, { wait: 3, goOn: true }),
                  timeout: 600,
                }),
              ]
            : [{ type: "text", text: "Noted." }],
      });
      await childOf(pane, pidFile);
      return pane;
    };

    // The first launch: the abort fails for the host's own reason, so the ladder takes its last
    // step and this worker ends itself with the status the daemon reads as a death.
    const first = await launch("stall-exit-1");
    const firstExit = await first.until("the last step of the ladder", async () =>
      stepOf(await first.records(), "exit-process")
    );
    expect(stepOf(await first.records(), "abort-turn")?.outcome).toContain(
      "the host's abort() failed: this host has no ctx.abort()"
    );
    expect(firstExit.outcome).toBe(`ending this process, stall exit 1 of ${STALL_EXIT_CAP}`);
    expect(await first.exited).toBe(STALL_EXIT_CODE);
    expect(JSON.parse(await readFile(exitsFile(state), "utf8"))).toMatchObject({
      exits: 1,
      who: `${ISSUE.toLowerCase()}-${ROLE}`,
    });

    // The daemon relaunches the claim in the same pod, so the next worker reads that count back
    // and spends the last exit the cap allows.
    const second = await launch("stall-exit-2");
    const secondExit = await second.until("the last step of the second launch", async () =>
      stepOf(await second.records(), "exit-process")
    );
    expect(secondExit.outcome).toBe(
      `ending this process, stall exit ${STALL_EXIT_CAP} of ${STALL_EXIT_CAP}`
    );
    expect(await second.exited).toBe(STALL_EXIT_CODE);
    expect(JSON.parse(await readFile(exitsFile(state), "utf8"))).toMatchObject({
      exits: STALL_EXIT_CAP,
    });

    // The launch after that is at the cap: it ends nothing, writes why, and hands its owner a
    // session that is stuck rather than a claim the daemon would fail.
    const third = await launch("stall-exit-3");
    const stopped = await third.until("the ladder stopping short", async () =>
      ofKind(await third.records(), "stopped").at(0)
    );
    expect(stopped.reason).toContain(
      `this worker has already ended itself ${STALL_EXIT_CAP} times for a stuck call`
    );
    expect(stopped.reason).toContain(`cap of ${STALL_EXIT_CAP}`);
    const told = await third.until("the notice that the ladder stopped short", () =>
      third.publishes().find((publish) => String(publish.message).includes("cannot recover"))
    );
    expect(told.topic).toBe(ARCHITECT_TOPIC);
    expect(String(told.message)).toContain("it cannot recover from on its own");
    expect(String(told.message)).toContain(`cap of ${STALL_EXIT_CAP}`);
    // The process is what the cap protects. A real clock is the only thing that shows it is still
    // there: an exit the ladder did not take emits no signal to await.
    await Bun.sleep(2 * STEP_MS);
    expect(third.running()).toBe(true);
    expect(JSON.parse(await readFile(exitsFile(state), "utf8"))).toMatchObject({
      exits: STALL_EXIT_CAP,
    });
  },
  3 * DEADLINE_MS
);

test.skipIf(skip)(
  "a counter file the worker cannot read ends no process: the ladder stops at its last step and tells its owner instead",
  async () => {
    const state = await scratchDir("unreadable-state");
    // A counter file this worker may not read: a count it cannot trust is one it will not end its
    // own process on.
    const file = exitsFile(state);
    await writeFile(
      file,
      `${JSON.stringify({ exits: 0, at: "2026-11-07T00:00:00Z", who: "x" })}\n`
    );
    await chmod(file, 0o000);
    const pidFile = path.join(await scratchDir("unreadable-pid"), "child.pid");
    const pane = await startPane({
      name: "stall-unreadable",
      stateDir: state,
      denyAbort: true,
      reply: (index) =>
        index === 1
          ? [
              call("eval", {
                language: "py",
                code: wedgeCell(pidFile, { wait: 3, goOn: true }),
                timeout: 600,
              }),
            ]
          : [{ type: "text", text: "Noted." }],
    });
    await childOf(pane, pidFile);

    const stopped = await pane.until("the ladder stopping short", async () =>
      ofKind(await pane.records(), "stopped").at(0)
    );
    expect(stopped.reason).toBe(
      "the stall exit count could not be read, so the process was not ended"
    );
    const told = await pane.until("the notice that the ladder stopped short", () =>
      pane.publishes().find((publish) => String(publish.message).includes("cannot recover"))
    );
    expect(String(told.message)).toContain("could not be read");
    // As above: the proposition is a process that is still there.
    await Bun.sleep(2 * STEP_MS);
    expect(pane.running()).toBe(true);
  },
  DEADLINE_MS + 60_000
);

test.skipIf(skip)(
  "a cell that finishes inside its bound is left alone: no record, no notice, no process ended, and its result comes back as usual",
  async () => {
    const state = await scratchDir("control-state");
    const pidFile = path.join(await scratchDir("control-pid"), "child.pid");
    const pane = await startPane({
      name: "stall-control",
      stateDir: state,
      reply: (index) =>
        index === 1
          ? [
              call("eval", {
                language: "py",
                code: [
                  "import pathlib, subprocess",
                  'p = subprocess.Popen(["/bin/sleep", "300"], start_new_session=True)',
                  `pathlib.Path(${JSON.stringify(pidFile)}).write_text(str(p.pid))`,
                  "print('the control started', p.pid)",
                ].join("\n"),
                timeout: 600,
              }),
            ]
          : [{ type: "text", text: "The control is done." }],
    });
    const child = await childOf(pane, pidFile);

    // The cell returned at once, so its result is the next turn's and nothing about it is overdue.
    const after = await pane.until("the turn that answers the control's result", () =>
      pane.turns().find((turn) => conversation(turn).includes("the control started"))
    );
    expect(conversation(after)).toContain("the control started");

    // Past the bound and two sweeps. A real clock, deliberately: the proposition is that the watch
    // did nothing at all, and a watch that does nothing emits no signal to await.
    await Bun.sleep(QUIET_MS);
    expect(await pane.records()).toEqual([]);
    expect(await pane.restarts()).toEqual([]);
    expect(pane.publishes()).toEqual([]);
    // The child the cell left running is still running: the first step ends the processes of a
    // call that is overdue, and no call here was.
    expect(await runs(child)).toBe(true);
    expect(pane.running()).toBe(true);
    await expect(readFile(exitsFile(state), "utf8")).rejects.toThrow();
  },
  DEADLINE_MS + 60_000
);

test.skipIf(skip)(
  "a cell with no child, on a kernel older than its call: the first step ends this session's own `eval` runner, which is the rule that needs the runner's real command",
  async () => {
    const state = await scratchDir("runner-state");
    const pane = await startPane({
      name: "stall-runner",
      stateDir: state,
      // The first cell starts the kernel and returns, so the second cell's runner is older than
      // the call that wedges: the one process rule 3 of `src/stall-recovery.ts` may end, and the
      // only thing left to end when a cell forks nothing.
      reply: (index) => {
        if (index === 1) {
          return [
            call("eval", { language: "py", code: "print('the kernel is ready')", timeout: 600 }),
          ];
        }
        if (index === 2) {
          return [
            call("eval", {
              language: "py",
              code: ["import time", "while True:", "    time.sleep(0.5)"].join("\n"),
              timeout: 600,
            }),
          ];
        }
        return [{ type: "text", text: "The call came back." }];
      },
    });

    const evidence = await pane.until("the evidence for the stuck call", async () =>
      ofKind(await pane.records(), "evidence").at(0)
    );
    if (!evidence.tree.read) throw new Error(`the tree was not read: ${evidence.tree.reason}`);
    // The runner as this host really writes it: a directory named for the user, holding a file
    // named for the kernel. The pattern that identified a runner stopped at the first path
    // separator, so it matched nothing here and this step ended nothing — which is what this case
    // caught and `STALL_TOOL_RUNNERS` now spans.
    const runner = evidence.tree.processes.find((process) =>
      process.command.includes("omp-python-runner-")
    );
    expect(runner?.command).toMatch(/omp-python-runner-[^\s]*\.py$/);
    expect(runner?.command).toContain("/runner-");

    const ended = await pane.until("the first step of the ladder", async () =>
      stepOf(await pane.records(), "end-processes")
    );
    expect(ended.outcome).toBe("ended 1 process of the call");
    expect(await runs(runner?.pid ?? -1)).toBe(false);

    // Ending the runner ends the call: the host hands back a dead kernel, so the ladder takes no
    // second step and this worker goes on with its work.
    const after = await pane.until("the turn that answers the call's result", () =>
      pane.turns().find((turn) => conversation(turn).includes("Kernel died"))
    );
    expect(conversation(after)).toContain("Kernel died");
    expect(ofKind(await pane.records(), "step").map((record) => record.step)).toEqual([
      "end-processes",
    ]);
    expect(pane.running()).toBe(true);
  },
  DEADLINE_MS + 60_000
);

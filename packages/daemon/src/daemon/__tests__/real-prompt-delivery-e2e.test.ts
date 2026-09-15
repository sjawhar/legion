// Real-tmux, real-process E2E for "a prompt is delivered only when a turn starts": no fake tmux
// runner and no fake WorkerRpcClient. Spawns a real `legion worker-shim` subprocess (via the real
// CLI entrypoint) inside a real tmux session around the `delayed-start-omp-rpc.ts` stand-in, whose
// three behaviours are the three ways a real OMP can answer a prompt: starts its turn (the healthy
// worker), acknowledges and never starts one (the fork build that swallows a message — LEGION-10),
// and starts it only after the daemon's wait has expired (a merely slow worker). The daemon side is
// the real `ProcessManager` over the real `TmuxRuntime`, so the retirement at the threshold really
// kills the pane. What this rig cannot do is boot a real OMP (the `/worker/started`/`/worker/ready`
// handshake lives in the pi-envoy plugin inside OMP), so the fresh-pane relaunch after a retirement
// is proven by the unit suite (processes.test.ts) and observed here only as the launch attempt.
import { afterAll, describe, expect, it, vi } from "bun:test";
import { existsSync } from "node:fs";
import { mkdtemp, readFile, rm } from "node:fs/promises";
import os from "node:os";
import path from "node:path";
import { roleToken } from "@legion/contracts";
import type { DaemonConfig } from "../config";
import { type LegionState, newLegionState, type WorkerRoleClaim } from "../legion-state";
import { parseProcStatStartTicks } from "../proc-stat";
import { locatorsForIssue, ProcessManager } from "../processes";
import type { TmuxLocator } from "../runtime";
import { TmuxRuntime, type TmuxRuntimeDeps } from "../runtime-tmux";
import { connectWorkerRpc } from "../worker-rpc";
import { fakeDispatchClient } from "./ci-fixtures";

const PROJECT = "realprompt";
const SESSION = "legion-smoke-T8Prompt";
/** The `ProcessManager` under test has `project: "realprompt"`, so every tmux command it issues
 * targets exactly this private socket; the fixture's own tmux calls must land on the same server. */
const TMUX_SOCKET = `legion-${PROJECT}`;
const tmuxArgv = (...rest: string[]) => ["tmux", "-L", TMUX_SOCKET, ...rest];
const DELAYED_START_OMP = path.join(
  import.meta.dir,
  "..",
  "..",
  "cli",
  "__tests__",
  "fixtures",
  "delayed-start-omp-rpc.ts"
);
const CLI_ENTRYPOINT = path.join(import.meta.dir, "..", "..", "cli", "index.ts");
/** The daemon's bound on a prompt's turn start in this rig — short, so a not-started attempt is
 * decided in about a second. */
const WORKER_RPC_TIMEOUT_SECONDS = 1;

const tempDirs: string[] = [];

async function scratchDir(): Promise<string> {
  const dir = await mkdtemp(path.join(os.tmpdir(), "legion-real-prompt-e2e-"));
  tempDirs.push(dir);
  return dir;
}

async function run(
  command: string[],
  options?: { cwd?: string; env?: NodeJS.ProcessEnv }
): Promise<{ stdout: string; stderr: string; exitCode: number }> {
  const proc = Bun.spawn(command, {
    stdout: "pipe",
    stderr: "pipe",
    ...(options?.cwd ? { cwd: options.cwd } : {}),
    ...(options?.env ? { env: options.env } : {}),
  });
  const [stdout, stderr, exitCode] = await Promise.all([
    new Response(proc.stdout).text(),
    new Response(proc.stderr).text(),
    proc.exited,
  ]);
  return { stdout, stderr, exitCode };
}

async function ensureSession(): Promise<void> {
  const listed = await run(tmuxArgv("has-session", "-t", SESSION));
  if (listed.exitCode !== 0) {
    const created = await run(
      tmuxArgv("new-session", "-d", "-s", SESSION, "-x", "200", "-y", "50")
    );
    if (created.exitCode !== 0) {
      throw new Error(`Unable to create tmux session ${SESSION}: ${created.stderr}`);
    }
  }
}

/** Opens a fresh window in the smoke session running `legion worker-shim` around the stand-in,
 * returning the locator a real spawn would record for it: window and (sole) pane id plus the
 * pane's root pid and that process's real `/proc/<pid>/stat` start ticks. */
async function openShimWindow(
  socketPath: string,
  env: Record<string, string>
): Promise<TmuxLocator & { panePid: number; paneStartTicks: number; tmuxPaneId: string }> {
  const envPairs = Object.entries(env).flatMap(([key, value]) => ["-e", `${key}=${value}`]);
  const shellCommand = `${process.execPath} ${CLI_ENTRYPOINT} worker-shim --socket ${socketPath} -- bun ${DELAYED_START_OMP}`;
  const opened = await run(
    tmuxArgv(
      "new-window",
      "-t",
      SESSION,
      "-P",
      "-F",
      "#{window_id} #{pane_id} #{pane_pid}",
      ...envPairs,
      shellCommand
    )
  );
  if (opened.exitCode !== 0) {
    throw new Error(`Unable to open shim window: ${opened.stderr}`);
  }
  const [windowId, paneId, pidToken] = opened.stdout.trim().split(" ");
  const panePid = Number(pidToken);
  if (!windowId || !paneId || !Number.isSafeInteger(panePid) || panePid <= 0) {
    throw new Error(`tmux new-window did not report ids: ${opened.stdout}`);
  }
  const paneStartTicks = parseProcStatStartTicks(await readFile(`/proc/${panePid}/stat`, "utf8"));
  return {
    runtime: "tmux",
    tmuxSession: SESSION,
    tmuxWindowId: windowId,
    tmuxPaneId: paneId,
    socketPath,
    panePid,
    paneStartTicks,
  };
}

// Real clock on purpose throughout this file: everything awaited below happens in another
// process — a tmux server reaping a pane, the stand-in emitting `agent_start` on its own timer,
// the daemon's own `worker_rpc_timeout_seconds` bound over a real shim — which no fake timer in
// this process can advance. Each poll awaits a named condition, never a guessed duration.

async function waitForSocket(target: string): Promise<void> {
  for (let attempt = 0; attempt < 200; attempt += 1) {
    if (existsSync(target)) return;
    await Bun.sleep(20);
  }
  throw new Error(`worker shim socket never appeared at ${target}`);
}

async function paneAlive(paneId: string): Promise<boolean> {
  const listed = await run(tmuxArgv("list-panes", "-a", "-F", "#{pane_id}"));
  return listed.stdout.split("\n").includes(paneId);
}

/** Polls until tmux itself no longer lists `paneId`, up to `timeoutMs` — tmux reaps a dead pane
 * out of its table a few milliseconds after the process exits, so a single check right after the
 * daemon's stop resolves would race that lag. Resolves `true` when the pane is gone. */
async function waitForPaneGone(paneId: string, timeoutMs = 5_000): Promise<boolean> {
  const deadline = Date.now() + timeoutMs;
  for (;;) {
    if (!(await paneAlive(paneId))) return true;
    if (Date.now() >= deadline) return false;
    await Bun.sleep(20);
  }
}

/** Polls `condition` on the real clock until it holds or `timeoutMs` passes. */
async function waitUntil(condition: () => boolean, timeoutMs: number): Promise<boolean> {
  const deadline = Date.now() + timeoutMs;
  while (!condition()) {
    if (Date.now() >= deadline) return false;
    await Bun.sleep(20);
  }
  return true;
}

async function promptLogLines(file: string): Promise<string[]> {
  if (!existsSync(file)) return [];
  return (await readFile(file, "utf8")).split("\n").filter((line) => line.length > 0);
}
async function waitForEvent(file: string, expected: string, timeoutMs = 5_000): Promise<boolean> {
  const deadline = Date.now() + timeoutMs;
  for (;;) {
    if ((await promptLogLines(file)).includes(expected)) return true;
    if (Date.now() >= deadline) return false;
    await Bun.sleep(20);
  }
}

function config(stateDir: string): DaemonConfig {
  return {
    project: PROJECT,
    legionId: "sjawhar/1",
    port: 0,
    runtime: { name: "tmux" },
    daemonUrl: "http://127.0.0.1:0",
    bind: "127.0.0.1",
    envoyUrl: "http://127.0.0.1:9020",
    natsUrls: ["nats://127.0.0.1:4222"],
    ompInvocation: "bun",
    ompLaunchPrefix: [],
    dispatchProject: "LEGSMOKE",
    repo: "sjawhar/legion",
    repos: ["sjawhar/legion"],
    admissionCap: 1,
    workerCap: 5,
    maxRecursionDepth: 8,
    lingerHours: 72,
    maxFixAttempts: 3,
    resyncIntervalMs: 600_000,
    workerStopTimeoutSeconds: 1,
    treeStopTimeoutSeconds: 1,
    workerBootTimeoutSeconds: 120,
    workerBootRegistrationDeadlineIntervals: 3,
    workerRpcTimeoutSeconds: WORKER_RPC_TIMEOUT_SECONDS,
    // Off: this rig's idle worker must stay resident for the whole case, not be retired by a
    // real clock between two drains.
    workerIdleRetireSeconds: 0,
    slowCommandTimeoutSeconds: 300,
    workerStreamPort: 13372,
    gates: { design: "off" },
    githubApps: {},
    stateDir,
  };
}

interface Rig {
  root: string;
  token: string;
  state: LegionState;
  processes: ProcessManager;
  opened: TmuxLocator & { tmuxPaneId: string };
  promptLog: string;
  eventLog: string;
  /** The OMP session file the claim's locator names — what a retirement carries into
   * `resumeSessionFile` for the `--resume` relaunch. */
  sessionFile: string;
  publications: string[];
  commands: string[][];
}

function claimOf(rig: Rig): WorkerRoleClaim {
  const claim = rig.state.roles[rig.token];
  if (!claim || !("issue" in claim)) throw new Error("tester claim disappeared");
  return claim;
}

/** A ready-confirmed, idle-live tester on a real shim with an architect assignment waiting on
 * the promotion queue, reconnected exactly as a daemon restart's `reconnectWorkers` would (the
 * real `get_state` seeds it idle) with launches enabled — the state every case starts from.
 * `fixtureEnv` selects the stand-in's behaviour. `jj`/`git` are refused by the runner: a
 * cold relaunch's workspace provisioning must never reach the network from a test. The one jj
 * command the prompt path itself runs — `adoptWorkingCopy`'s `jj metaedit` on the issue's working
 * copy, before every assignment prompt — is answered as a no-op instead: this rig has no
 * workspace, and a refused adoption would fail the delivery the cases below are about. */
async function rig(root: string, fixtureEnv: Record<string, string>): Promise<Rig> {
  await ensureSession();
  const stateDir = await scratchDir();
  const socketPath = path.join(stateDir, "tester.sock");
  const promptLog = path.join(stateDir, "prompts.log");
  const eventLog = path.join(stateDir, "events.log");
  const sessionFile = path.join(stateDir, "tester-session.json");
  const opened = await openShimWindow(socketPath, {
    FIXTURE_PROMPT_LOG: promptLog,
    FIXTURE_EVENT_LOG: eventLog,
    ...fixtureEnv,
  });
  await waitForSocket(socketPath);
  expect(await paneAlive(opened.tmuxPaneId)).toBe(true);

  const state = newLegionState(PROJECT, 1);
  state.issues[root] = { key: root, title: "Root", status: "in_progress", children: [] };
  state.trees[root] = { root, generation: 1, status: "active", launchFailures: 0 };
  const token = roleToken(PROJECT, root, "tester");
  state.roles[token] = {
    issue: root,
    role: "tester",
    generation: 1,
    sessionId: "ses_tester",
    readyConfirmedAt: Date.now(),
    pendingAssignment: {
      kind: "assignment",
      task: "verify #41",
      queuedAt: "2026-08-24T00:00:00.000Z",
      deliveryId: "00000000-0000-4000-8000-000000000039",
    },
    locator: { ...opened, ompSessionFile: sessionFile },
  };
  state.workerAdmission.queue.push(token);

  const cfg = config(stateDir);
  const commands: string[][] = [];
  const publications: string[] = [];
  const runner: TmuxRuntimeDeps["run"] = async (command, options) => {
    commands.push(command);
    if (command[0] === "jj" && command[1] === "metaedit") {
      return { stdout: "", stderr: "Nothing changed.\n", exitCode: 0 };
    }
    if (command[0] === "jj" || command[0] === "git") {
      return { stdout: "", stderr: "workspace provisioning is not part of this rig", exitCode: 1 };
    }
    return run(command, options);
  };
  const runtime = new TmuxRuntime({
    tmux: { run: runner, socket: TMUX_SOCKET },
    project: PROJECT,
    stateDir,
    ompInvocation: cfg.ompInvocation,
    ompLaunchPrefix: cfg.ompLaunchPrefix,
    provisioningToken: async () => "installation-token",
    run: runner,
    repo: cfg.repo,
    credentialHelper: "!true",
    slowCommandTimeoutMs: cfg.slowCommandTimeoutSeconds * 1000,
    connectWorkerRpc: (socket) => connectWorkerRpc(socket, cfg.workerRpcTimeoutSeconds * 1000),
    workerRpcTimeoutMs: () => cfg.workerRpcTimeoutSeconds * 1000,
    now: () => Date.now(),
    issueLocators: (issue) => locatorsForIssue(state, issue),
  });
  const processes = new ProcessManager({
    state,
    saveState: async () => {},
    config: cfg,
    runtime,
    run: runner,
    processPath: process.env.PATH ?? "",
    rolePromptsDir: path.resolve(import.meta.dir, "../../../../pi-envoy/roles"),
    credentialHelper: "!true",
    publishRole: (_subject, json) => {
      publications.push(json);
    },
    natsRequest: async () => JSON.stringify({ type: "ack" }),
    mintControllerCapability: async () => "controller-secret",
    mintBootToken: async () => "boot-token",
    mintWorkerBootToken: async () => "worker-boot-token",
    workerCatchup: {
      baseEnv: {},
      runner: async () => ({ stdout: "[]", stderr: "", exitCode: 0 }),
      tokenManager: {
        getToken: async () => ({
          token: "worker-token",
          expiresAt: "2099-01-01T00:00:00.000Z",
          gitIdentity: {
            name: "legion-implement[bot]",
            email: "implement@users.noreply.github.com",
          },
        }),
      },
      repo: "sjawhar/legion",
    },
    now: () => Date.now(),
    dispatchClient: fakeDispatchClient(),
    revokeSessionCapability: () => {},
  });
  await processes.reconnectWorkers();
  processes.enableLaunches();
  return {
    root,
    token,
    state,
    processes,
    opened,
    promptLog,
    eventLog,
    sessionFile,
    publications,
    commands,
  };
}

const workerStarted = (root: string) =>
  JSON.stringify({ type: "worker-started", issue: root, role: "tester" });

afterAll(async () => {
  await run(tmuxArgv("kill-server"));
  await Promise.all(tempDirs.splice(0).map((dir) => rm(dir, { recursive: true, force: true })));
});

describe("real prompt delivery (tmux + worker-shim, no mocks)", () => {
  it.skipIf(process.env.LEGION_E2E !== "1")(
    "commits a queued assignment to a real idle worker once its shim reports agent_start",
    async () => {
      const t = await rig("LEGION-9101", { FIXTURE_AGENT_START_DELAY_MS: "0" });
      try {
        await t.processes.reconcileWorkerAdmission();

        expect(t.state.workerAdmission.queue).toEqual([]);
        expect(claimOf(t).pendingAssignment).toBeUndefined();
        expect(claimOf(t).promptFailures).toBe(0);
        expect(t.state.phases[t.root]).toEqual({
          phase: "tester",
          sessionId: "ses_tester",
          // The rig runs on the real clock; the stamp's exact value is the daemon's Date.now().
          assignedAt: expect.any(String),
        });
        expect(await promptLogLines(t.promptLog)).toEqual(["verify #41"]);
        expect(t.publications.filter((json) => json === workerStarted(t.root))).toHaveLength(1);
        expect(await paneAlive(t.opened.tmuxPaneId)).toBe(true);
      } finally {
        await run(tmuxArgv("kill-pane", "-t", t.opened.tmuxPaneId));
      }
    },
    30_000
  );

  it.skipIf(process.env.LEGION_E2E !== "1")(
    "keeps the assignment queued and counts each acknowledgement no turn follows, retiring the real pane at the third and relaunching cold",
    async () => {
      const t = await rig("LEGION-9102", {});
      const errorLog = vi.spyOn(console, "error").mockImplementation(() => {});
      let logged: string[] = [];
      const deliveryIds: string[] = [];
      try {
        for (let attempt = 1; attempt <= 3; attempt += 1) {
          await t.processes.reconcileWorkerAdmission();
          expect(t.state.workerAdmission.queue).toEqual([t.token]);
          expect(claimOf(t).pendingAssignment).toMatchObject({
            kind: "assignment",
            task: "verify #41",
            queuedAt: "2026-08-24T00:00:00.000Z",
          });
          const deliveryId = claimOf(t).pendingAssignment?.deliveryId;
          if (!deliveryId) throw new Error("no-turn retry did not receive a delivery ID");
          deliveryIds.push(deliveryId);
          expect([...new Set(deliveryIds)]).toHaveLength(attempt);
          expect(claimOf(t).promptFailures).toBe(attempt === 3 ? 0 : attempt);
          expect(t.state.phases[t.root]).toBeUndefined();
          expect(await promptLogLines(t.promptLog)).toHaveLength(attempt);
          logged = errorLog.mock.calls.map((call) => call.map(String).join(" "));
          expect(
            logged.filter(
              (line) =>
                line.includes(`failed to prompt queued worker ${t.token}`) &&
                line.includes(
                  `acknowledged the prompt but started no turn within ${WORKER_RPC_TIMEOUT_SECONDS * 1000}ms (get_state: isStreaming=false)`
                )
            )
          ).toHaveLength(attempt);
        }

        // The third failure retired the worker: the real pane is gone, the locator cleared with
        // the session file carried for the relaunch, the assignment still queued, the first
        // relaunch cycle counted on the claim and logged (LEGION-93: cycle 1 of a bound of 2 —
        // a relaunch, not yet `worker-died`).
        expect(await waitForPaneGone(t.opened.tmuxPaneId)).toBe(true);
        expect(claimOf(t).locator).toBeUndefined();
        expect(claimOf(t).resumeSessionFile).toBe(t.sessionFile);
        expect(claimOf(t).promptRetires).toBe(1);
        expect(claimOf(t).promptFailures).toBe(0);
        expect(t.state.workerAdmission.queue).toEqual([t.token]);
        expect(t.publications.filter((json) => json === workerStarted(t.root))).toHaveLength(0);
        expect(t.publications.filter((json) => json.includes('"worker-died"'))).toHaveLength(0);
        expect(
          logged.filter(
            (line) =>
              line ===
              `[legion] ${t.token}: retiring after 3 prompts with no turn started; relaunch cycle 1 (bound 2)`
          )
        ).toHaveLength(1);

        // Whichever drain reaches the queue first — the one the retired socket's own close
        // triggers (`onWorkerClientClosed` -> reconnect refused -> `markWorkerDead`), or this
        // explicit one — takes the cold-launch branch, never a fourth prompt. This rig refuses
        // workspace provisioning, so each attempt lands as a launch failure on the claim.
        await t.processes.reconcileWorkerAdmission();
        expect(await promptLogLines(t.promptLog)).toHaveLength(3);
        expect(claimOf(t).launchFailures).toBeGreaterThanOrEqual(1);
        expect(t.commands.some((command) => command[0] === "jj")).toBe(true);
        expect(t.state.workerAdmission.queue).toEqual([t.token]);
      } finally {
        errorLog.mockRestore();
        if (await paneAlive(t.opened.tmuxPaneId)) {
          await run(tmuxArgv("kill-pane", "-t", t.opened.tmuxPaneId));
        }
      }
    },
    30_000
  );

  it.skipIf(process.env.LEGION_E2E !== "1")(
    "replays a started delivery after its turn completes before the prompt response times out",
    async () => {
      const t = await rig("LEGION-9104", {
        FIXTURE_AGENT_START_DELAY_MS: "10",
        FIXTURE_PROMPT_RESPONSE_DELAY_MS: "1500",
      });
      try {
        await t.processes.reconcileWorkerAdmission();
        expect(t.state.workerAdmission.queue).toEqual([t.token]);
        expect(claimOf(t).pendingAssignment).toMatchObject({
          kind: "assignment",
          task: "verify #41",
        });
        expect(claimOf(t).promptFailures).toBe(1);
        expect(await promptLogLines(t.promptLog)).toEqual(["verify #41"]);
        expect(await waitForEvent(t.eventLog, "delivery:end")).toBe(true);

        await t.processes.reconcileWorkerAdmission();

        expect(t.state.workerAdmission.queue).toEqual([]);
        expect(claimOf(t).pendingAssignment).toBeUndefined();
        expect(claimOf(t).promptFailures).toBe(0);
        expect(t.state.phases[t.root]).toMatchObject({ phase: "tester", sessionId: "ses_tester" });
        expect(await promptLogLines(t.promptLog)).toEqual(["verify #41"]);
      } finally {
        if (await paneAlive(t.opened.tmuxPaneId)) {
          await run(tmuxArgv("kill-pane", "-t", t.opened.tmuxPaneId));
        }
      }
    },
    10_000
  );
  it.skipIf(process.env.LEGION_E2E !== "1")(
    "forwards the retry after a foreign turn makes the first prompt refuse",
    async () => {
      const t = await rig("LEGION-9105", {
        FIXTURE_INITIAL_FOREIGN_TURN_DELAY_MS: "1000",
        FIXTURE_AGENT_START_DELAY_MS: "0",
      });
      let client: Awaited<ReturnType<typeof connectWorkerRpc>> | undefined;
      try {
        expect(await waitForEvent(t.eventLog, "foreign:start")).toBe(true);
        const socketPath = t.opened.socketPath;
        if (!socketPath) throw new Error("shim socket was not recorded");
        client = await connectWorkerRpc(socketPath, 1_000);
        await expect(
          client.prompt("verify #41", "00000000-0000-4000-8000-000000000105")
        ).rejects.toThrow("AgentBusyError");
        expect(await waitForEvent(t.eventLog, "foreign:end")).toBe(true);

        const retry = await client.prompt("verify #41", "00000000-0000-4000-8000-000000000105");
        await retry.turnStarted;
        expect(await promptLogLines(t.promptLog)).toEqual(["verify #41", "verify #41"]);
      } finally {
        client?.close();
        if (await paneAlive(t.opened.tmuxPaneId)) {
          await run(tmuxArgv("kill-pane", "-t", t.opened.tmuxPaneId));
        }
      }
    },
    10_000
  );
  it.skipIf(process.env.LEGION_E2E !== "1")(
    "commits a turn that starts after the wait expired as the same delivery, with no second prompt",
    async () => {
      const t = await rig("LEGION-9103", { FIXTURE_AGENT_START_DELAY_MS: "2500" });
      const errorLog = vi.spyOn(console, "error").mockImplementation(() => {});
      const infoLog = vi.spyOn(console, "info").mockImplementation(() => {});
      try {
        await t.processes.reconcileWorkerAdmission();
        expect(t.state.workerAdmission.queue).toEqual([t.token]);
        expect(claimOf(t).promptFailures).toBe(1);
        expect(t.state.phases[t.root]).toBeUndefined();

        // No further drain: the worker's own late agent_start is what commits the delivery.
        expect(await waitUntil(() => t.state.workerAdmission.queue.length === 0, 4_000)).toBe(true);
        expect(claimOf(t).pendingAssignment).toBeUndefined();
        expect(claimOf(t).promptFailures).toBe(0);
        expect(t.state.phases[t.root]).toEqual({
          phase: "tester",
          sessionId: "ses_tester",
          // The rig runs on the real clock; the stamp's exact value is the daemon's Date.now().
          assignedAt: expect.any(String),
        });
        expect(t.publications.filter((json) => json === workerStarted(t.root))).toHaveLength(1);
        expect(await promptLogLines(t.promptLog)).toEqual(["verify #41"]);
        expect(infoLog.mock.calls.map((call) => String(call[0]))).toContainEqual(
          expect.stringContaining(`${t.token} started its turn after the prompt wait expired`)
        );

        // A following drain finds nothing queued: the task is never prompted a second time.
        await t.processes.reconcileWorkerAdmission();
        expect(await promptLogLines(t.promptLog)).toEqual(["verify #41"]);
      } finally {
        errorLog.mockRestore();
        infoLog.mockRestore();
        await run(tmuxArgv("kill-pane", "-t", t.opened.tmuxPaneId));
      }
    },
    30_000
  );
});

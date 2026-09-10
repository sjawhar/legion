import { afterAll, afterEach, describe, expect, it, vi } from "bun:test";
import { mkdir, mkdtemp, readFile, rm, writeFile } from "node:fs/promises";
import os from "node:os";
import path from "node:path";
import {
  controllerToken,
  type IssueKey,
  type LegionRole,
  roleToken,
  roleTopic,
} from "@legion/contracts";
import { spawnCapabilityKey } from "../api/auth";
import type { DaemonConfig } from "../config";
import type { ExceptionInfo } from "../events";
import {
  type LegionState,
  loadState as legionStateLoadState,
  saveState as legionStateSaveState,
  newLegionState,
} from "../legion-state";
import {
  addressingFragment,
  type ControlDirective,
  ProcessManager,
  type ProcessManagerDeps,
  StopFailed,
  TreeClosingError,
} from "../processes";
import type { WorkerRpcClient } from "../worker-rpc";
import { fakeDispatchClient } from "./ci-fixtures";

const root = "LEGION-42";
const child = "LEGION-43";
const grandchild = "LEGION-44";
const tempDirs: string[] = [];
const liveManagers: ProcessManager[] = [];

// Every `armBootWatchdog` fired by a `spawnWorker`/`launchWorker` call in these tests races the
// real `deps.sleep` (never overridden here) up to the full `workerBootTimeoutSeconds`. Without
// this, a test that spawns a worker and never separately retires/closes it leaves a live
// multi-minute `setTimeout` pending, which keeps the test process alive long after its
// assertions finish. `dispose()` cancels every such watchdog outright.
afterEach(() => {
  for (const instance of liveManagers.splice(0)) instance.dispose();
});

/**
 * Yields once to the event loop's macrotask queue (never a wall-clock-bound wait — `setImmediate`
 * fires on the next tick, whatever that costs). A boot watchdog's `sleep` override must yield the
 * same way: a pure microtask loop (`Promise.resolve()`) never lets the real `mkdir`/`writeFile`
 * calls `provisionWorkspace` makes during a retry actually complete, since a chain that keeps
 * re-queuing its own microtasks monopolizes the microtask queue and starves every macrotask —
 * including the very I/O the retry is waiting on.
 */
function onceEventLoop(): Promise<void> {
  const { promise, resolve } = Promise.withResolvers<void>();
  setImmediate(resolve);
  return promise;
}

/** Drains an async chain built entirely from injected fake `sleep`/`now` (never a real timer) by
 * yielding the event loop repeatedly, so a boot watchdog's connect-retry/retire loop settles
 * deterministically without guessing a real wall-clock wait. */
async function flushEventLoop(ticks = 2_000): Promise<void> {
  for (let tick = 0; tick < ticks; tick += 1) {
    await onceEventLoop();
  }
}

/** As `flushEventLoop`, but stops as soon as `condition` is met rather than a fixed tick count —
 * for a chain whose length in event-loop ticks before some expected effect isn't known exactly. */
async function flushEventLoopUntil(condition: () => boolean, maxTicks = 5_000): Promise<void> {
  for (let tick = 0; tick < maxTicks && !condition(); tick += 1) {
    await onceEventLoop();
  }
}

async function temporaryDir(): Promise<string> {
  const directory = await mkdtemp(path.join(os.tmpdir(), "legion-processes-"));
  tempDirs.push(directory);
  return directory;
}

type FakeWorkerRpcClient = WorkerRpcClient & {
  prompts: string[];
  negotiated: boolean;
  getStateCalls: number;
  getStateImpl?: () => Promise<Record<string, unknown>>;
  idleFireCount: number;
  emitRunState(state: "running" | "idle"): void;
  /** Sets `runState` directly, bypassing the idle trigger entirely — models a real client's
   * post-rejection restore (an undo, never a transition; see `WorkerRpcClient.prompt`'s doc
   * comment), as opposed to `emitRunState`, which fires `onIdle` on a genuine idle transition. */
  setRunStateSilently(state: "unknown" | "running" | "idle"): void;
};
function fakeWorkerRpcClient(): FakeWorkerRpcClient {
  const closed = Promise.withResolvers<void>();
  let idleCallback: (() => void) | undefined;
  let runState: "unknown" | "running" | "idle" = "unknown";
  const client = {
    closed: closed.promise,
    get runState() {
      return runState;
    },
    prompts: [] as string[],
    negotiated: false,
    getStateCalls: 0,
    getStateImpl: undefined as (() => Promise<Record<string, unknown>>) | undefined,
    idleFireCount: 0,
    async negotiate() {
      client.negotiated = true;
    },
    async prompt(message: string) {
      runState = "running";
      client.prompts.push(message);
    },
    async getState() {
      client.getStateCalls += 1;
      return client.getStateImpl ? client.getStateImpl() : {};
    },
    shutdown() {
      // Mirrors the real shim: the frame alone never closes the socket — the shim closes it only
      // once OMP actually exits, asynchronously relative to receiving the frame. Deferred by a
      // microtask (never synchronous) so a test asserting the graceful path is decided by real
      // promise ordering against `stopProcess`'s timeout race, not by `closed` already having
      // settled before that race was even built.
      queueMicrotask(() => client.close());
    },
    close() {
      const wasIdle = runState === "idle";
      runState = "idle";
      closed.resolve();
      if (!wasIdle) {
        client.idleFireCount += 1;
        idleCallback?.();
      }
    },
    onIdle(callback: () => void) {
      idleCallback = callback;
    },
    emitRunState(state: "running" | "idle") {
      const wasIdle = runState === "idle";
      runState = state;
      if (state === "idle" && !wasIdle) {
        client.idleFireCount += 1;
        idleCallback?.();
      }
    },
    setRunStateSilently(state: "unknown" | "running" | "idle") {
      runState = state;
    },
  };
  return client;
}
function config(stateDir: string, overrides: Partial<DaemonConfig> = {}): DaemonConfig {
  return {
    project: "omp",
    legionId: "sjawhar/1",
    port: 13999,
    envoyUrl: "http://127.0.0.1:9020",
    natsUrls: ["nats://127.0.0.1:4222"],
    ompInvocation: "mise x github:sjawhar/oh-my-pi@18.0.3-sami.20260824-002841 -- omp",
    ompLaunchPrefix: [],
    dispatchProject: "LEGSMOKE",
    repo: "sjawhar/legion",
    repos: ["sjawhar/legion"],
    appLogins: [],
    admissionCap: 1,
    workerCap: 5,
    maxRecursionDepth: 8,
    lingerHours: 2,
    maxFixAttempts: 3,
    resyncIntervalMs: 600_000,
    workerStopTimeoutSeconds: 10,
    treeStopTimeoutSeconds: 60,
    workerBootTimeoutSeconds: 120,
    workerBootRegistrationDeadlineIntervals: 3,
    workerRpcTimeoutSeconds: 5,
    gates: { design: "root-issues", merge: "human" },
    githubApps: {},
    stateDir,
    ...overrides,
  };
}

function tree(state: LegionState, issue: IssueKey = root, generation = 1) {
  state.trees[issue] = {
    root: issue,
    generation,
    locator: {
      tmuxSession: "legion-omp",
      tmuxWindowId: "@42",
      tmuxPaneId: "%0",
      socketPath: "/state/workers/architect.sock",
      ompSessionFile: "/state/trees/sjawhar-legion-42/.omp/session.json",
    },
    status: "active",
    launchFailures: 0,
  };
}

function exception(
  role: string,
  original = {
    topic: "notifications.github.sjawhar.legion.issue.42.comment",
    payload: '{"body":"retry"}',
    eventId: "evt-1",
  }
): ExceptionInfo {
  return { roleToken: role, reason: "no_holder", original };
}

function liveRun(command: string[]): Promise<{ stdout: string; exitCode: number }> {
  if (command[0] === "tmux" && command[1] === "list-windows") {
    return Promise.resolve({ stdout: "sjawhar-legion-42\n", exitCode: 0 });
  }
  if (command[0] === "tmux" && command[1] === "list-panes") {
    return Promise.resolve({ stdout: "12345\n", exitCode: 0 });
  }
  if (command[0] === "kill") return Promise.resolve({ stdout: "", exitCode: 0 });
  return Promise.resolve({ stdout: "", exitCode: 0 });
}

function manager(
  state = newLegionState("omp", 1),
  options: Partial<
    ProcessManagerDeps & {
      readProcessCmdline?: (pid: number) => Promise<string>;
    }
  > = {},
  { skipEnablePromotion = false }: { skipEnablePromotion?: boolean } = {}
): {
  manager: ProcessManager;
  state: LegionState;
  commands: string[][];
  controlRequests: Array<{ subject: string; json: string }>;
  publications: Array<{ subject: string; json: string }>;
  revokedSessions: string[];
} {
  const commands: string[][] = [];
  const publications: Array<{ subject: string; json: string }> = [];
  const controlRequests: Array<{ subject: string; json: string }> = [];
  const revokedSessions: string[] = [];
  const { run: requestedRun, ...overrides } = options;
  let launchedAnyWindow = false;
  const commandRunner =
    requestedRun ??
    (async (command: string[]) => {
      commands.push(command);
      // A liveness probe of a previously-recorded window happens before this test's first
      // successful new-window/split-window; once one has succeeded, tmux's own `-P -F` output
      // already reports the pane id and pid synchronously, so no further discovery call happens.
      if (
        command[0] === "tmux" &&
        command[1] === "list-panes" &&
        (command.includes("#{pane_id}") || command.includes("#{pane_pid}"))
      ) {
        if (!launchedAnyWindow) return { stdout: "", exitCode: 1 };
        return {
          stdout: command.includes("#{pane_id}") ? "%1\n" : "12345\n",
          exitCode: 0,
        };
      }
      if (command[0] === "tmux" && command[1] === "split-window") {
        launchedAnyWindow = true;
        return { stdout: "%2 12345\n", exitCode: 0 };
      }
      if (command[0] === "tmux" && command[1] === "new-window") {
        launchedAnyWindow = true;
        return { stdout: "@42 %1 12345\n", exitCode: 0 };
      }
      return { stdout: "", exitCode: 0 };
    });
  const deps: ProcessManagerDeps = {
    state,
    saveState: async () => {},
    config: config("/state"),
    natsPublish: (subject, json) => publications.push({ subject, json }),
    natsRequest: async (subject, json) => {
      controlRequests.push({ subject, json });
      return JSON.stringify({ type: "ack" });
    },
    mintControllerCapability: async () => "controller-secret",
    mintBootToken: async () => "boot-token",
    mintWorkerBootToken: async () => "worker-boot-token",
    connectWorkerRpc: async () => fakeWorkerRpcClient(),
    provisioningToken: async () => "daemon-installation-token",
    statPrompt: async () => {},
    ompInvocation: "/opt/oh-my-pi/18.0.3/omp",
    readProcessCmdline: async () => "omp\0",
    panePath: "/full/bin:/usr/bin",
    credentialHelper: "!/opt/legion/bun /opt/legion/cli/index.ts credential",
    workerCatchup: {
      repo: "sjawhar/legion",
      runner: async () => ({ stdout: "[]", stderr: "", exitCode: 0 }),
      tokenManager: {
        getToken: async () => ({
          token: "worker-token",
          expiresAt: "2099-01-01T00:00:00.000Z",
          gitIdentity: {
            name: "legion-implement[bot]",
            email: "42+legion-implement[bot]@users.noreply.github.com",
          },
        }),
      },
    },
    now: () => Date.parse("2026-08-24T00:00:00.000Z"),
    dispatchClient: fakeDispatchClient(),
    revokeSessionCapability: (sessionId) => revokedSessions.push(sessionId),
    ...overrides,
    run: async (command, runnerOptions) => {
      const result = await commandRunner(command, runnerOptions);
      if (result.exitCode !== 0) return result;
      if (command[0] === "jj" && command[1] === "git" && command[2] === "clone") {
        const cloneDir = command[4];
        if (!cloneDir) throw new Error("Jujutsu clone is missing its destination");
        await mkdir(path.join(cloneDir, ".jj"), { recursive: true });
      }
      if (command[0] === "jj" && command[1] === "workspace" && command[2] === "add") {
        const workspaceDir = command[3];
        if (!workspaceDir) throw new Error("Jujutsu workspace is missing its destination");
        await mkdir(workspaceDir, { recursive: true });
      }
      if (
        command[0] === "tmux" &&
        (command[1] === "new-window" || command[1] === "new-session") &&
        result.stdout.trim() === ""
      ) {
        return { ...result, stdout: "@42\n" };
      }
      return result;
    },
  };
  const processManager = new ProcessManager(deps);
  liveManagers.push(processManager);
  // Every existing test exercises worker-queue promotion as already "booted" (index.ts calls
  // this immediately after `api` is assigned) — only the dedicated boot-ordering test passes
  // `skipEnablePromotion` to exercise the gate itself.
  if (!skipEnablePromotion) processManager.enableWorkerPromotion();
  return {
    manager: processManager,
    state,
    commands,
    controlRequests,
    publications,
    revokedSessions,
  };
}

/** Common setup for running-worker-cap tests: a fresh single-root-tree `LegionState` wired to
 * `manager()` at the given `workerCap`, with a scratch `stateDir` already provisioned — cuts the
 * `temporaryDir`/`newLegionState`/`tree`/`config` boilerplate those tests otherwise repeat. Not
 * every `workerCap`-configured test in this file uses it: several earlier
 * dead-worker/`--resume`/launch-failure-threshold tests need bespoke workspace-directory,
 * `ompSessionFile`, or mock-command setup this fixture's minimal footprint does not cover, and
 * are left as their own hand-rolled `manager()` calls rather than forced onto it. Callers seed
 * `state.roles`/`state.workerAdmission.queue` on the returned `state` before touching
 * `processes`; `overrides` merges into (and can replace) any of `manager()`'s own deps. */
async function workerCapFixture(
  workerCap: number,
  overrides: Partial<
    ProcessManagerDeps & { readProcessCmdline?: (pid: number) => Promise<string> }
  > = {},
  { skipEnablePromotion = false }: { skipEnablePromotion?: boolean } = {}
) {
  const stateDir = await temporaryDir();
  const state = newLegionState("omp", 1);
  tree(state);
  const {
    manager: processes,
    state: managedState,
    commands,
    publications,
    controlRequests,
  } = manager(
    state,
    { config: config(stateDir, { workerCap }), ...overrides },
    { skipEnablePromotion }
  );
  return { processes, state, managedState, commands, publications, controlRequests, stateDir };
}

function tmuxWindowEnvironment(command: readonly string[]): Record<string, string> {
  const environment: Record<string, string> = {};
  for (let index = 0; index < command.length; index += 1) {
    if (command[index] !== "-e") continue;
    const assignment = command[index + 1];
    if (!assignment) throw new Error("tmux -e is missing an environment assignment");
    const separator = assignment.indexOf("=");
    if (separator === -1) throw new Error(`tmux environment assignment is invalid: ${assignment}`);
    environment[assignment.slice(0, separator)] = assignment.slice(separator + 1);
  }
  return environment;
}

afterAll(async () => {
  await Promise.all(tempDirs.map((directory) => rm(directory, { recursive: true, force: true })));
});

describe("ProcessManager", () => {
  it("admits only up to the configured global cap and selects the next queued tree on release", async () => {
    const stateDir = await temporaryDir();
    let windows = 0;
    let completeSpawns: (() => void) | undefined;
    const spawned = new Promise<void>((resolve) => {
      completeSpawns = resolve;
    });
    const { manager: processes, state } = manager(newLegionState("omp", 1), {
      config: config(stateDir),
      run: async (command) => {
        if (command[1] === "new-window") {
          if (++windows === 2) completeSpawns?.();
          return { stdout: "@42 %1 12345\n", exitCode: 0 };
        }
        return { stdout: "", exitCode: 0 };
      },
    });

    expect(processes.admit(root)).toBe("spawned");
    expect(processes.admit(child)).toBe("queued");
    expect(state.admission).toEqual({ cap: 1, active: [root], queue: [child] });

    processes.releaseSlot(root);
    await spawned;

    expect(state.admission).toEqual({ cap: 1, active: [child], queue: [] });
  });

  it("drainSpawns awaits an admit-triggered spawn that admit itself never awaits, including that spawn's own saveState", async () => {
    const stateDir = await temporaryDir();
    const saveGate = Promise.withResolvers<void>();
    const { manager: processes } = manager(newLegionState("omp", 1), {
      config: config(stateDir),
      run: async (command) => {
        if (command[1] === "has-session") return { stdout: "", exitCode: 0 };
        if (command[1] === "new-window") return { stdout: "@42 %1 4242\n", exitCode: 0 };
        return { stdout: "", exitCode: 0 };
      },
      saveState: async () => {
        await saveGate.promise;
      },
    });

    // admit()'s "spawned" path fires startRoot without awaiting it — the
    // spawn, and its own post-success saveState, are still running when
    // admit() returns.
    expect(processes.admit(root)).toBe("spawned");

    let drained = false;
    const draining = processes.drainSpawns().then(() => {
      drained = true;
    });

    // saveGate is still pending, so the tracked spawn cannot have settled
    // yet — no real wait needed to know `drained` is still false here.
    expect(drained).toBe(false);

    saveGate.resolve();
    await draining;
    expect(drained).toBe(true);
  });

  it("provisions the root issue workspace before launching OMP in that workspace", async () => {
    const stateDir = await temporaryDir();
    const repo = path.join(stateDir, "repos", "github.com", "sjawhar", "legion");
    const workspace = path.join(stateDir, "workspaces", "sjawhar", "legion", "legion-42");
    await mkdir(path.join(repo, ".jj"), { recursive: true });
    const workspaceCalls: Array<{
      readonly command: string[];
      readonly opts:
        | {
            readonly cwd?: string;
            readonly env?: Readonly<Record<string, string>>;
          }
        | undefined;
    }> = [];
    const {
      manager: processes,
      state,
      commands,
    } = manager(newLegionState("omp", 1), {
      config: config(stateDir, {
        dispatchUrl: "http://127.0.0.1:18766",
        dispatchToken: "test-dispatch-token",
      }),
      run: async (
        command: string[],
        opts?: {
          readonly cwd?: string;
          readonly env?: Readonly<Record<string, string>>;
        }
      ) => {
        commands.push(command);
        workspaceCalls.push({ command, opts });
        if (command[0] === "jj" && command[1] === "workspace" && command[2] === "add") {
          await mkdir(workspace, { recursive: true });
        }
        if (command[1] === "has-session") return { stdout: "", exitCode: 1 };
        if (command[1] === "new-window") return { stdout: "@42 %1 12345\n", exitCode: 0 };
        return { stdout: "", exitCode: 0 };
      },
    });

    state.issues[root] = { key: root, title: "Root", status: "todo", children: [] };
    state.admission.active.push(root);
    state.trees[root] = { root, generation: 0, status: "active", launchFailures: 0 };
    await processes.spawnRoot(root);

    expect(await readFile(path.join(workspace, ".omp", "config.yml"), "utf8")).toBe("");
    expect(state.trees[root]).toMatchObject({
      generation: 1,
      status: "active",
    });
    expect(commands).toEqual([
      ["jj", "git", "fetch", "-R", repo],
      ["git", `--git-dir=${repo}/.git`, "worktree", "prune"],
      [
        "jj",
        "workspace",
        "add",
        workspace,
        "--name",
        "legion-42",
        "--revision",
        "main",
        "-R",
        repo,
      ],
      ["jj", "bookmark", "set", "legion/LEGION-42", "--allow-backwards"],
      ...[
        ["git", `--git-dir=${repo}/.git`, "config", "--replace-all", "credential.helper", ""],
        [
          "git",
          `--git-dir=${repo}/.git`,
          "config",
          "--add",
          "credential.helper",
          "!/opt/legion/bun /opt/legion/cli/index.ts credential",
        ],
        [
          "git",
          `--git-dir=${repo}/.git`,
          "config",
          "--replace-all",
          "credential.https://github.com.helper",
          "",
        ],
        [
          "git",
          `--git-dir=${repo}/.git`,
          "config",
          "--add",
          "credential.https://github.com.helper",
          "!/opt/legion/bun /opt/legion/cli/index.ts credential",
        ],
        ["git", `--git-dir=${repo}/.git`, "config", "credential.interactive", "false"],
      ],
      ["tmux", "has-session", "-t", "legion-omp"],
      ["tmux", "new-session", "-d", "-s", "legion-omp", "-n", "__legion_bootstrap", "sleep 3600"],
      ["tmux", "set-option", "-t", "legion-omp", "@legion_owner", "legion-omp"],
      [
        "tmux",
        "new-window",
        "-P",
        "-F",
        "#{window_id} #{pane_id} #{pane_pid}",
        "-t",
        "legion-omp",
        "-n",
        "legion-42",
        "-e",
        "LEGION_TREE=LEGION-42",
        "-e",
        "LEGION_ISSUE=LEGION-42",
        "-e",
        "LEGION_ROLE=architect",
        "-e",
        `LEGION_ROOT_WORKSPACE=${workspace}`,
        "-e",
        "LEGION_GENERATION=1",
        "-e",
        "LEGION_BOOT_TOKEN=boot-token",
        "-e",
        "LEGION_DAEMON_URL=http://127.0.0.1:13999",
        "-e",
        "LEGION_PROJECT=omp",
        "-e",
        "ENVOY_NATS_URL=nats://127.0.0.1:4222",
        "-e",
        "ENVOY_URL=http://127.0.0.1:9020",
        "-e",
        "LEGION_CONTROL_SUBJECT=legion.ctl.legion-42.1",
        "-e",
        "LEGION_MAX_RECURSION_DEPTH=8",
        "-e",
        `LEGION_STATE_DIR=${stateDir}`,
        "-e",
        "LEGION_CREDENTIAL_HELPER=!/opt/legion/bun /opt/legion/cli/index.ts credential",
        "-e",
        "GIT_CONFIG_COUNT=0",
        "-e",
        "GIT_TERMINAL_PROMPT=0",
        "-e",
        "PATH=/full/bin:/usr/bin",
        "-e",
        "DISPATCH_URL=http://127.0.0.1:18766",
        "-e",
        "DISPATCH_TOKEN=test-dispatch-token",
        `cd ${workspace} && ${process.execPath} ${path.resolve(import.meta.dir, "../../cli/index.ts")} worker-shim --socket ${path.join(stateDir, "workers", "architect-9e2fb104.sock")} -- /opt/oh-my-pi/18.0.3/omp --mode rpc --append-system-prompt "$(cat ${path.resolve(import.meta.dir, "../../../../pi-envoy")}/roles/architect-root.md)" --append-system-prompt '${addressingFragment("omp", root, root, "architect").replaceAll("'", "'\\''")}'`,
      ],
      ["tmux", "kill-window", "-t", "legion-omp:__legion_bootstrap"],
      ["tmux", "set-option", "-w", "-t", "@42", "@legion_owner", "legion-omp"],
    ]);
    expect(workspaceCalls).toContainEqual({
      command: ["jj", "bookmark", "set", "legion/LEGION-42", "--allow-backwards"],
      opts: { cwd: workspace },
    });
    expect(workspaceCalls).toContainEqual({
      command: ["jj", "git", "fetch", "-R", repo],
      opts: {
        env: {
          GIT_ASKPASS: expect.stringMatching(/provisioning-credential-.+\/askpass$/),
          GIT_TERMINAL_PROMPT: "0",
          LEGION_PROVISIONING_TOKEN: "daemon-installation-token",
        },
      },
    });
  });
  it("launches controller and root windows with disjoint exact environments", async () => {
    const stateDir = await temporaryDir();
    let sessionExists = false;
    const { manager: processes, commands } = manager(newLegionState("omp", 1), {
      config: config(stateDir),
      run: async (command) => {
        commands.push(command);
        if (command[1] === "has-session") {
          return { stdout: "", exitCode: sessionExists ? 0 : 1 };
        }
        if (command[1] === "new-session") sessionExists = true;
        if (command[1] === "new-window") return { stdout: "@42 %1 12345\n", exitCode: 0 };
        return { stdout: "", exitCode: 0 };
      },
    });

    await processes.ensureController();
    await processes.spawnRoot(root);

    const windows = commands.filter((command) => command[1] === "new-window");
    expect(windows).toHaveLength(2);
    const controllerWindow = windows[0];
    const rootWindow = windows[1];
    if (!controllerWindow || !rootWindow) throw new Error("missing Legion tmux windows");

    expect(tmuxWindowEnvironment(controllerWindow)).toEqual({
      LEGION_CONTROLLER: "1",
      LEGION_ROLE: "controller",
      LEGION_CONTROLLER_SECRET: "controller-secret",
      LEGION_DAEMON_URL: "http://127.0.0.1:13999",
      LEGION_PROJECT: "omp",
      ENVOY_NATS_URL: "nats://127.0.0.1:4222",
      ENVOY_URL: "http://127.0.0.1:9020",
      PATH: "/full/bin:/usr/bin",
    });
    expect(tmuxWindowEnvironment(rootWindow)).toEqual({
      LEGION_TREE: root,
      LEGION_ISSUE: root,
      LEGION_ROLE: "architect",
      LEGION_ROOT_WORKSPACE: path.join(stateDir, "workspaces", "sjawhar", "legion", "legion-42"),
      LEGION_GENERATION: "1",
      LEGION_BOOT_TOKEN: "boot-token",
      LEGION_DAEMON_URL: "http://127.0.0.1:13999",
      LEGION_PROJECT: "omp",
      ENVOY_NATS_URL: "nats://127.0.0.1:4222",
      ENVOY_URL: "http://127.0.0.1:9020",
      LEGION_CONTROL_SUBJECT: "legion.ctl.legion-42.1",
      LEGION_MAX_RECURSION_DEPTH: "8",
      LEGION_STATE_DIR: stateDir,
      LEGION_CREDENTIAL_HELPER: "!/opt/legion/bun /opt/legion/cli/index.ts credential",
      GIT_CONFIG_COUNT: "0",
      GIT_TERMINAL_PROMPT: "0",
      PATH: "/full/bin:/usr/bin",
    });
  });
  it("writes the tree's Dispatch status to in_progress on a successful spawn, then to done on close", async () => {
    const stateDir = await temporaryDir();
    const statusWrites: Array<{ issue: IssueKey; status: string }> = [];
    let sessionExists = false;
    const state = newLegionState("omp", 1);
    state.issues[root] = { key: root, title: "Root", status: "todo", children: [] };
    state.admission.active.push(root);
    state.trees[root] = { root, generation: 0, status: "active", launchFailures: 0 };
    const { manager: processes, state: managedState } = manager(state, {
      config: config(stateDir),
      dispatchClient: fakeDispatchClient({
        setStatus: async (issue, status) => {
          statusWrites.push({ issue, status });
        },
      }),
      run: async (command) => {
        if (command[1] === "has-session") return { stdout: "", exitCode: sessionExists ? 0 : 1 };
        if (command[1] === "new-session") sessionExists = true;
        if (command[1] === "new-window") return { stdout: "@42 %1 12345\n", exitCode: 0 };
        return { stdout: "", exitCode: 0 };
      },
    });

    await processes.spawnRoot(root);

    expect(statusWrites).toEqual([{ issue: root, status: "in_progress" }]);
    expect(managedState.trees[root]?.status).toBe("active");

    await processes.closeTree(root);

    expect(statusWrites).toEqual([
      { issue: root, status: "in_progress" },
      { issue: root, status: "done" },
    ]);
    expect(managedState.trees[root]?.status).toBe("closed");
  });
  it("skips the close-time Dispatch done write when the issue is already closed", async () => {
    // Reproduces a real Dispatch server's behavior: `reduceIssueClosed` applies a closed issue's
    // `status: "done"` onto `state.issues` before `closeTree` ever runs (via `expireLinger`'s
    // linger, or `reportRootExit`'s own `status === "done"` gate) — so by the time `closeTree`
    // gets here, Dispatch already considers the issue closed and permanently refuses a further
    // status PATCH on it. Writing "done" again must not be attempted.
    const stateDir = await temporaryDir();
    const statusWrites: Array<{ issue: IssueKey; status: string }> = [];
    let sessionExists = false;
    const state = newLegionState("omp", 1);
    state.issues[root] = { key: root, title: "Root", status: "done", children: [] };
    tree(state);
    const { manager: processes, state: managedState } = manager(state, {
      config: config(stateDir),
      dispatchClient: fakeDispatchClient({
        setStatus: async (issue, status) => {
          statusWrites.push({ issue, status });
        },
      }),
      run: async (command) => {
        if (command[1] === "has-session") return { stdout: "", exitCode: sessionExists ? 0 : 1 };
        if (command[1] === "new-session") sessionExists = true;
        if (command[1] === "new-window") return { stdout: "@42 %1 12345\n", exitCode: 0 };
        return { stdout: "", exitCode: 0 };
      },
    });

    await processes.closeTree(root);

    expect(statusWrites).toEqual([]);
    expect(managedState.trees[root]?.status).toBe("closed");
    expect(managedState.pendingStatusWrites[root]).toBeUndefined();
  });
  it("closes a parked root on linger expiry without overwriting Dispatch with done", async () => {
    const state = newLegionState("omp", 1);
    state.issues[root] = { key: root, title: "Parked root", status: "backlog", children: [] };
    tree(state);
    state.trees[root].status = "lingering";
    const statusWrites: Array<{ issue: IssueKey; status: string }> = [];
    const { manager: processes, state: managedState } = manager(state, {
      dispatchClient: fakeDispatchClient({
        setStatus: async (issue, status) => {
          statusWrites.push({ issue, status });
        },
      }),
    });

    await processes.expireLinger(root);

    expect(managedState.trees[root]?.status).toBe("closed");
    expect(statusWrites).toEqual([]);
  });

  it("retires a just-opened root pane without a status write when a human parks it during launch", async () => {
    const stateDir = await temporaryDir();
    const state = newLegionState("omp", 1);
    state.issues[root] = { key: root, title: "Racing root", status: "todo", children: [] };
    state.trees[root] = { root, generation: 0, status: "active", launchFailures: 0 };
    state.admission.active.push(root);
    const launchStarted = Promise.withResolvers<void>();
    const launchGate = Promise.withResolvers<void>();
    const commands: string[][] = [];
    const statusWrites: Array<{ issue: IssueKey; status: string }> = [];
    const { manager: processes, state: managedState } = manager(state, {
      config: config(stateDir),
      sleep: async () => {},
      connectWorkerRpc: async () => {
        throw new Error("ECONNREFUSED");
      },
      dispatchClient: fakeDispatchClient({
        setStatus: async (issue, status) => {
          statusWrites.push({ issue, status });
        },
      }),
      run: async (command) => {
        commands.push(command);
        if (command[0] === "tmux" && command[1] === "new-window") {
          launchStarted.resolve();
          await launchGate.promise;
          return { stdout: "@42 %1 12345\n", exitCode: 0 };
        }
        return { stdout: "", exitCode: 0 };
      },
    });

    const spawning = processes.spawnRoot(root);
    await launchStarted.promise;
    state.issues[root].status = "backlog";
    state.trees[root].status = "lingering";
    state.admission.active.splice(state.admission.active.indexOf(root), 1);
    launchGate.resolve();

    await spawning;

    expect(managedState.trees[root]).toMatchObject({ status: "lingering" });
    expect(managedState.trees[root]?.locator).toBeUndefined();
    expect(statusWrites).toEqual([]);
    expect(commands).toContainEqual(["tmux", "kill-pane", "-t", "%1"]);
  });

  it("proceeds to a running root when a delayed in_progress echo lands mid-launch", async () => {
    const stateDir = await temporaryDir();
    const state = newLegionState("omp", 1);
    state.issues[root] = { key: root, title: "Racing root", status: "todo", children: [] };
    state.trees[root] = { root, generation: 0, status: "active", launchFailures: 0 };
    state.admission.active.push(root);
    const launchStarted = Promise.withResolvers<void>();
    const launchGate = Promise.withResolvers<void>();
    const commands: string[][] = [];
    const statusWrites: Array<{ issue: IssueKey; status: string }> = [];
    const { manager: processes, state: managedState } = manager(state, {
      config: config(stateDir),
      sleep: async () => {},
      connectWorkerRpc: async () => {
        throw new Error("ECONNREFUSED");
      },
      dispatchClient: fakeDispatchClient({
        setStatus: async (issue, status) => {
          statusWrites.push({ issue, status });
        },
      }),
      run: async (command) => {
        commands.push(command);
        if (command[0] === "tmux" && command[1] === "new-window") {
          launchStarted.resolve();
          await launchGate.promise;
          return { stdout: "@42 %1 12345\n", exitCode: 0 };
        }
        return { stdout: "", exitCode: 0 };
      },
    });

    const spawning = processes.spawnRoot(root);
    await launchStarted.promise;
    // This daemon's own earlier `writeStatus(..., "in_progress")` lands through Dispatch's echo
    // while this launch is still in flight -- the launch's own write hasn't run yet.
    state.issues[root].status = "in_progress";
    launchGate.resolve();

    await spawning;

    expect(managedState.trees[root]).toMatchObject({ status: "active" });
    expect(managedState.trees[root]?.locator).toBeDefined();
    expect(statusWrites).toEqual([{ issue: root, status: "in_progress" }]);
    expect(commands).not.toContainEqual(["tmux", "kill-pane", "-t", "%1"]);
  });

  it("retires an older generation's launch as stale when a park-then-re-admit starts a newer one first", async () => {
    const stateDir = await temporaryDir();
    const state = newLegionState("omp", 1);
    state.issues[root] = { key: root, title: "Racing root", status: "todo", children: [] };
    state.trees[root] = { root, generation: 0, status: "active", launchFailures: 0 };
    state.admission.active.push(root);
    const firstLaunchStarted = Promise.withResolvers<void>();
    const releaseFirstLaunch = Promise.withResolvers<void>();
    const commands: string[][] = [];
    const statusWrites: Array<{ issue: IssueKey; status: string }> = [];
    const { manager: processes, state: managedState } = manager(state, {
      config: config(stateDir),
      sleep: async () => {},
      connectWorkerRpc: async () => {
        throw new Error("ECONNREFUSED");
      },
      dispatchClient: fakeDispatchClient({
        setStatus: async (issue, status) => {
          statusWrites.push({ issue, status });
        },
      }),
      run: async (command) => {
        commands.push(command);
        if (command[0] === "tmux" && command[1] === "new-window") {
          firstLaunchStarted.resolve();
          await releaseFirstLaunch.promise;
          return { stdout: "@42 %1 12345\n", exitCode: 0 };
        }
        if (command[0] === "tmux" && command[1] === "split-window") {
          return { stdout: "%2 54321\n", exitCode: 0 };
        }
        if (
          command[0] === "tmux" &&
          command[1] === "list-panes" &&
          command.includes("#{pane_id}")
        ) {
          return { stdout: "%1\n", exitCode: 0 };
        }
        if (
          command[0] === "tmux" &&
          command[1] === "list-panes" &&
          command.includes("#{pane_pid}")
        ) {
          return { stdout: "12345\n", exitCode: 0 };
        }
        return { stdout: "", exitCode: 0 };
      },
    });

    const firstSpawn = processes.spawnRoot(root);
    await firstLaunchStarted.promise;

    // Human parks the issue while the first launch (generation 1) is still blocked in tmux,
    // releasing the admission slot and lingering the tree -- exactly as the reducer's linger
    // effect would.
    await processes.beginLinger(root);
    // ...then it is re-admitted immediately, starting generation 2's own launch (queued behind
    // generation 1's still-open tmux call, via `launchShimmedProcess`'s per-issue serialize
    // lane) before the stale generation-1 launch ever returns.
    state.issues[root].status = "todo";
    expect(processes.admit(root)).toBe("spawned");

    // Only now does the older, generation-1 launch's tmux call finally resolve; generation 2's
    // queued launch runs immediately after it, splitting a second pane into the same window.
    releaseFirstLaunch.resolve();
    await firstSpawn;
    await processes.drainSpawns();

    expect(statusWrites).toEqual([{ issue: root, status: "in_progress" }]);
    expect(managedState.trees[root]).toMatchObject({
      generation: 2,
      status: "active",
      locator: { tmuxWindowId: "@42", tmuxPaneId: "%2" },
    });
    expect(commands).toContainEqual(["tmux", "kill-pane", "-t", "%1"]);
  });

  it("resurrects a dead root whose Dispatch status is in_progress instead of treating it as a human park", async () => {
    const stateDir = await temporaryDir();
    const sessionFile = path.join(stateDir, "architect-session.json");
    await writeFile(sessionFile, "{}", "utf8");
    const state = newLegionState("omp", 1);
    tree(state);
    const locator = state.trees[root].locator;
    if (!locator) throw new Error("test root is missing a locator");
    state.trees[root].locator = { ...locator, ompSessionFile: sessionFile };
    state.issues[root] = { key: root, title: "Root", status: "in_progress", children: [] };
    const statusWrites: Array<{ issue: IssueKey; status: string }> = [];
    const { manager: processes, state: managedState } = manager(state, {
      config: config(stateDir),
      dispatchClient: fakeDispatchClient({
        setStatus: async (issue, status) => {
          statusWrites.push({ issue, status });
        },
      }),
    });

    await processes.resurrect(root);

    expect(managedState.trees[root]).toMatchObject({ status: "active" });
    expect(managedState.trees[root]?.locator).toBeDefined();
    expect(statusWrites).toEqual([{ issue: root, status: "in_progress" }]);
  });

  it("marks a closed tree lingering again when its stale-pane retirement fails, instead of stranding an unreapable locator", async () => {
    const stateDir = await temporaryDir();
    const state = newLegionState("omp", 1);
    state.issues[root] = { key: root, title: "Racing root", status: "todo", children: [] };
    state.trees[root] = { root, generation: 0, status: "active", launchFailures: 0 };
    state.admission.active.push(root);
    const launchStarted = Promise.withResolvers<void>();
    const launchGate = Promise.withResolvers<void>();
    const { manager: processes, state: managedState } = manager(state, {
      config: config(stateDir),
      sleep: async () => {},
      connectWorkerRpc: async () => {
        throw new Error("ECONNREFUSED");
      },
      run: async (command) => {
        if (command[0] === "tmux" && command[1] === "new-window") {
          launchStarted.resolve();
          await launchGate.promise;
          return { stdout: "@42 %1 12345\n", exitCode: 0 };
        }
        if (command[0] === "tmux" && command[1] === "kill-pane") {
          return { stdout: "", exitCode: 1, stderr: "tmux: server not responding" };
        }
        return { stdout: "", exitCode: 0 };
      },
    });

    const spawning = processes.spawnRoot(root);
    await launchStarted.promise;
    // A concurrent close finishes for this exact tree before the post-launch check runs.
    state.trees[root].status = "closed";
    launchGate.resolve();

    await spawning;

    expect(managedState.trees[root]).toMatchObject({
      status: "lingering",
      lingerUntil: "2026-08-24T00:00:00.000Z",
    });
    expect(managedState.trees[root]?.locator).toMatchObject({ tmuxPaneId: "%1" });
  });

  it("records a failed Dispatch status write in pendingStatusWrites without throwing, for both spawn and close", async () => {
    const stateDir = await temporaryDir();
    let sessionExists = false;
    const state = newLegionState("omp", 1);
    state.trees[root] = { root, generation: 0, status: "active", launchFailures: 0 };
    state.issues[root] = { key: root, title: "Root", status: "todo", children: [] };
    state.admission.active.push(root);
    const { manager: processes, state: managedState } = manager(state, {
      config: config(stateDir),
      dispatchClient: fakeDispatchClient({
        setStatus: async () => {
          throw new Error("Dispatch unavailable");
        },
      }),
      run: async (command) => {
        if (command[1] === "has-session") return { stdout: "", exitCode: sessionExists ? 0 : 1 };
        if (command[1] === "new-session") sessionExists = true;
        if (command[1] === "new-window") return { stdout: "@42 %1 12345\n", exitCode: 0 };
        return { stdout: "", exitCode: 0 };
      },
    });

    await processes.spawnRoot(root);

    expect(managedState.trees[root]?.status).toBe("active");
    expect(managedState.pendingStatusWrites[root]).toEqual({
      status: "in_progress",
      statusAtRecord: "todo",
    });

    await processes.closeTree(root);

    expect(managedState.trees[root]?.status).toBe("closed");
    expect(managedState.pendingStatusWrites[root]).toEqual({
      status: "done",
      statusAtRecord: "todo",
    });
  });
  it("gives collision-prone issue paths distinct escaped cosmetic window names", async () => {
    const stateDir = await temporaryDir();
    const left = "ORGLEGIONSMOKE-1";
    const right = "LEGIONSMOKE-1";
    const { manager: processes, commands } = manager(newLegionState("omp", 2), {
      config: config(stateDir, { admissionCap: 2 }),
    });

    await processes.spawnRoot(left);
    await processes.spawnRoot(right);

    const names = commands
      .filter((command) => command[0] === "tmux" && command.includes("-n"))
      .map((command) => command[command.indexOf("-n") + 1]);
    expect(names).toEqual(["orglegionsmoke-1", "legionsmoke-1"]);
  });

  it("caps an escaped cosmetic window name", async () => {
    const stateDir = await temporaryDir();
    const issue = `${"B".repeat(200)}-1`;
    const { manager: processes, commands } = manager(newLegionState("omp", 1), {
      config: config(stateDir),
    });

    await processes.spawnRoot(issue);

    const window = commands.find((command) => command[0] === "tmux" && command.includes("-n"));
    expect(window?.[window.indexOf("-n") + 1]).toHaveLength(160);
  });

  it("keeps the worker socket path under the Unix socket length limit for a very long issue key", async () => {
    const stateDir = await temporaryDir();
    const issue = `${"B".repeat(200)}-1`;
    const { manager: processes, commands } = manager(newLegionState("omp", 1), {
      config: config(stateDir),
    });

    await processes.spawnRoot(issue);

    const window = commands.find((command) => command[0] === "tmux" && command.includes("-n"));
    const shellCommand = window?.at(-1);
    const socketMatch = shellCommand?.match(/--socket (\S+)/);
    if (!socketMatch?.[1]) throw new Error("launch command is missing its --socket argument");
    expect(Buffer.byteLength(socketMatch[1])).toBeLessThan(100);
  });

  it("records a new tmux window id and probes that id rather than its cosmetic name", async () => {
    const stateDir = await temporaryDir();
    const commands: string[][] = [];
    const { manager: processes, state } = manager(newLegionState("omp", 1), {
      config: config(stateDir),
      run: async (command) => {
        commands.push(command);
        if (command[1] === "has-session") return { stdout: "", exitCode: 0 };
        if (command[1] === "new-window") return { stdout: "@314 %7 12345\n", exitCode: 0 };
        if (command[1] === "list-panes" && command.includes("%7")) {
          return { stdout: "12345\n", exitCode: 0 };
        }
        if (command[0] === "kill") return { stdout: "", exitCode: 0 };
        return { stdout: "sjawhar-legion-42\n", exitCode: 0 };
      },
    });

    state.issues[root] = { key: root, title: "Root", status: "todo", children: [] };
    state.admission.active.push(root);
    state.trees[root] = { root, generation: 0, status: "active", launchFailures: 0 };
    await processes.spawnRoot(root);

    expect(state.trees[root]?.locator).toMatchObject({
      tmuxSession: "legion-omp",
      tmuxWindowId: "@314",
      tmuxPaneId: "%7",
    });
    expect(await processes.probe(root)).toBe("alive");
    expect(commands).toContainEqual(["tmux", "list-panes", "-t", "%7", "-F", "#{pane_pid}"]);
  });

  it("rejects a live pane whose process command is not OMP", async () => {
    const state = newLegionState("omp", 1);
    tree(state);
    const locator = state.trees[root]?.locator;
    if (!locator) throw new Error("test root is missing a locator");
    locator.tmuxWindowId = "@314";
    const { manager: processes } = manager(state, {
      readProcessCmdline: async () => "bash\0",
      run: liveRun,
    });

    expect(await processes.probe(root)).toBe("dead");
  });

  it("kills only stale daemon-owned windows that are not state locators", async () => {
    const state = newLegionState("omp", 1);
    tree(state);
    state.controllerLocator = {
      tmuxSession: "legion-omp",
      tmuxWindowId: "@43",
    };
    const commands: string[][] = [];
    const activitySeconds = Date.parse("2026-08-24T00:00:00.000Z") / 1000;
    const { manager: processes } = manager(state, {
      run: async (command) => {
        commands.push(command);
        if (command[1] === "list-windows") {
          return {
            stdout: [
              `@42\tlegion-omp\t${activitySeconds}`,
              `@43\tlegion-omp\t${activitySeconds}`,
              `@99\tlegion-omp\t${activitySeconds - 121}`,
              `@100\t\t${activitySeconds - 121}`,
            ].join("\n"),
            exitCode: 0,
          };
        }
        return { stdout: "", exitCode: 0 };
      },
    });

    await processes.reconcileTmuxWindows();

    expect(commands).toContainEqual(["tmux", "kill-window", "-t", "@99"]);
    expect(commands).not.toContainEqual(["tmux", "kill-window", "-t", "@100"]);
  });

  it("clears a self-reporting root's locator without attempting to stop it (it is the caller, still alive and blocked on this response)", async () => {
    const state = newLegionState("omp", 1);
    tree(state);
    if (!state.trees[root]?.locator) throw new Error("test root is missing a locator");
    state.admission.active.push(root);
    const { manager: processes, commands } = manager(state);

    await processes.markProcessDead(root);

    expect(state.trees[root]?.locator).toBeUndefined();
    expect(state.trees[root]?.status).toBe("dead");
    expect(commands).toEqual([]);
  });

  it("passes the packaged role prompt path to OMP for root and controller windows, without an --extension flag", async () => {
    const stateDir = await temporaryDir();
    const ompInvocation = "/opt/oh-my-pi/18.0.3/omp";
    const panePath = "/full/bin:/usr/bin";
    const { manager: processes, commands } = manager(newLegionState("omp", 1), {
      config: config(stateDir),
      ompInvocation,
      panePath,
      run: async (command) => {
        commands.push(command);
        if (command[1] === "new-window") return { stdout: "@42 %1 12345\n", exitCode: 0 };
        return { stdout: "", exitCode: 0 };
      },
    });

    await processes.spawnRoot(root);
    await processes.ensureController();

    const workspaceDir = path.join(stateDir, "workspaces", "sjawhar", "legion", "legion-42");
    const controllerDir = path.join(stateDir, "controller");
    const extensionDir = path.resolve(import.meta.dir, "../../../../pi-envoy");
    const entrypoint = path.resolve(import.meta.dir, "../../cli/index.ts");
    const socketPath = path.join(stateDir, "workers", "architect-9e2fb104.sock");
    const controllerSocketPath = path.join(stateDir, "workers", "controller.sock");
    const windows = commands.filter((command) => command[1] === "new-window");
    expect(windows.map((command) => command.at(-1))).toEqual([
      `cd ${workspaceDir} && ${process.execPath} ${entrypoint} worker-shim --socket ${socketPath} -- ${ompInvocation} --mode rpc --append-system-prompt "$(cat ${extensionDir}/roles/architect-root.md)" --append-system-prompt '${addressingFragment("omp", root, root, "architect").replaceAll("'", "'\\''")}'`,
      `cd ${controllerDir} && ${process.execPath} ${entrypoint} worker-shim --socket ${controllerSocketPath} -- ${ompInvocation} --mode rpc --append-system-prompt "$(cat ${extensionDir}/roles/controller-root.md)"`,
    ]);
    expect(windows.map((command) => command.includes(`PATH=${panePath}`))).toEqual([true, true]);
  });

  it("prepends the configured omp_launch_prefix before the OMP invocation for root and controller windows", async () => {
    const stateDir = await temporaryDir();
    const ompInvocation = "/opt/oh-my-pi/18.0.3/omp";
    const { manager: processes, commands } = manager(newLegionState("omp", 1), {
      config: config(stateDir, { ompLaunchPrefix: ["secrets", "ANTHROPIC_API_KEY", "--"] }),
      ompInvocation,
      run: async (command) => {
        commands.push(command);
        if (command[1] === "new-window") return { stdout: "@42 %1 12345\n", exitCode: 0 };
        return { stdout: "", exitCode: 0 };
      },
    });

    await processes.spawnRoot(root);
    await processes.ensureController();

    const windows = commands.filter((command) => command[1] === "new-window");
    for (const command of windows) {
      expect(command.at(-1)).toContain(
        `-- secrets ANTHROPIC_API_KEY -- ${ompInvocation} --mode rpc`
      );
    }
    expect(windows).toHaveLength(2);
  });

  it("prepends the configured omp_launch_prefix before the OMP invocation for a phase worker's window", async () => {
    const state = newLegionState("omp", 1);
    tree(state);
    state.issues[child] = {
      key: child,
      title: "Child",
      status: "in_progress",
      parent: root,
      children: [],
    };
    const stateDir = await temporaryDir();
    const ompInvocation = "/opt/oh-my-pi/18.0.3/omp";
    const { manager: processes, commands } = manager(state, {
      config: config(stateDir, { ompLaunchPrefix: ["secrets", "ANTHROPIC_API_KEY", "--"] }),
      ompInvocation,
    });

    await processes.spawnWorker(root, child, "implementer", "implement it");

    const windows = commands.filter(
      (command) => command[1] === "new-window" || command[1] === "split-window"
    );
    expect(windows.length).toBeGreaterThan(0);
    for (const command of windows) {
      expect(command.at(-1)).toContain(
        `-- secrets ANTHROPIC_API_KEY -- ${ompInvocation} --mode rpc`
      );
    }
  });

  it("rolls back a failed tmux launch instead of retaining an active tree or admission slot", async () => {
    const stateDir = await temporaryDir();
    const state = newLegionState("omp", 1);
    state.admission.active.push(root);
    let saves = 0;
    const { manager: processes } = manager(state, {
      config: config(stateDir),
      saveState: async () => {
        saves += 1;
      },
      run: async (command) =>
        command[1] === "new-window"
          ? { stdout: "window creation failed", exitCode: 1 }
          : { stdout: "", exitCode: 0 },
    });

    await expect(processes.spawnRoot(root)).rejects.toThrow("tmux new-window failed");

    expect(state.trees[root]).toEqual({
      root,
      generation: 0,
      status: "queued",
      launchFailures: 1,
    });
    expect(state.admission).toEqual({ cap: 1, active: [], queue: [root] });
    expect(saves).toBeGreaterThan(0);
  });

  it("rolls back a failed tmux session creation before recording a root locator", async () => {
    const stateDir = await temporaryDir();
    const state = newLegionState("omp", 1);
    state.admission.active.push(root);
    const { manager: processes, commands } = manager(state, {
      config: config(stateDir),
      run: async (command) => {
        if (command[1] === "has-session") return { stdout: "", exitCode: 1 };
        if (command[1] === "new-session") return { stdout: "session creation failed", exitCode: 1 };
        return { stdout: "", exitCode: 0 };
      },
    });

    await expect(processes.spawnRoot(root)).rejects.toThrow("tmux new-session failed");

    expect(commands.some((command) => command[1] === "new-window")).toBeFalse();
    expect(state.trees[root]).toEqual({
      root,
      generation: 0,
      status: "queued",
      launchFailures: 1,
    });
    expect(state.admission).toEqual({ cap: 1, active: [], queue: [root] });
  });

  it("fails a root launch before tmux when its architect prompt is missing", async () => {
    const stateDir = await temporaryDir();
    const state = newLegionState("omp", 1);
    state.admission.active.push(root);
    const { manager: processes, commands } = manager(state, {
      config: config(stateDir),
      statPrompt: async (promptPath) => {
        throw new Error(`Missing prompt: ${promptPath}`);
      },
    });

    await expect(processes.spawnRoot(root)).rejects.toThrow("Missing prompt");

    expect(commands.some((command) => command[0] === "tmux")).toBeFalse();
    expect(state.trees[root]).toEqual({
      root,
      generation: 0,
      status: "queued",
      launchFailures: 1,
    });
    expect(state.admission).toEqual({ cap: 1, active: [], queue: [root] });
  });

  it("promotes the next queued tree when a failed launch releases capacity", async () => {
    const stateDir = await temporaryDir();
    const state = newLegionState("omp", 1);
    state.admission.active.push(root);
    state.admission.queue.push(child);
    let completePromotion: (() => void) | undefined;
    const promoted = new Promise<void>((resolve) => {
      completePromotion = resolve;
    });
    const { manager: processes } = manager(state, {
      config: config(stateDir),
      run: async (command) => {
        if (command[1] === "new-window" && command.includes(`LEGION_TREE=${root}`)) {
          return { stdout: "root launch failed", exitCode: 1 };
        }
        if (command[1] === "new-window" && command.includes(`LEGION_TREE=${child}`)) {
          completePromotion?.();
          return { stdout: "@2 %2 4242\n", exitCode: 0 };
        }
        return { stdout: "", exitCode: 0 };
      },
    });

    await expect(processes.spawnRoot(root)).rejects.toThrow("tmux new-window failed");
    await promoted;

    expect(state.admission).toEqual({ cap: 1, active: [child], queue: [root] });
  });

  it("attempts each queued tree once in a bounded promotion sweep when launches keep failing", async () => {
    const stateDir = await temporaryDir();
    const state = newLegionState("omp", 1);
    state.admission.active.push(root);
    state.admission.queue.push(child);
    let attempts = 0;
    let finishSweep: (() => void) | undefined;
    const sweepFinished = new Promise<void>((resolve) => {
      finishSweep = resolve;
    });
    const { manager: processes } = manager(state, {
      config: config(stateDir),
      run: async (command) => {
        if (command[1] !== "new-window") return { stdout: "", exitCode: 0 };
        attempts += 1;
        return { stdout: "window creation failed", exitCode: 1 };
      },
    });

    const originalConsoleError = console.error;
    console.error = () => {
      finishSweep?.();
    };
    try {
      await expect(processes.spawnRoot(root)).rejects.toThrow("tmux new-window failed");
      await sweepFinished;

      expect(attempts).toBe(2);
      expect(state.trees[root]).toMatchObject({
        status: "queued",
        launchFailures: 1,
      });
      expect(state.trees[child]).toMatchObject({
        status: "queued",
        launchFailures: 1,
      });
    } finally {
      console.error = originalConsoleError;
    }
  });

  it("promotes queued trees into slots opened by a cap raised between restarts", async () => {
    const stateDir = await temporaryDir();
    const state = newLegionState("omp", 1);
    state.admission.queue.push(root, child);
    const { manager: processes } = manager(state, {
      config: config(stateDir, { admissionCap: 2 }),
    });

    state.issues[root] = { key: root, title: "Root", status: "todo", children: [] };
    state.issues[child] = { key: child, title: "Child", status: "todo", children: [] };
    await processes.reconcileAdmission();

    expect(state.admission).toEqual({ cap: 2, active: [root, child], queue: [] });
    expect(state.trees[root]?.status).toBe("active");
    expect(state.trees[child]?.status).toBe("active");
  });

  it("demotes a persisted active tree with no recorded locator back to queued and re-spawns it", async () => {
    const stateDir = await temporaryDir();
    const state = newLegionState("omp", 1);
    // Simulates a crash between advancePromotionSweep's persist() (which
    // marks a promoted tree "active") and startRoot ever recording a
    // locator: on disk, the tree is active but nothing is running.
    state.trees[root] = {
      root,
      generation: 1,
      status: "active",
      launchFailures: 0,
    };
    state.admission.active.push(root);
    state.issues[root] = { key: root, title: "Root", status: "todo", children: [] };
    const errorLog = vi.spyOn(console, "error").mockImplementation(() => {});
    const { manager: processes } = manager(state, {
      config: config(stateDir),
      run: async (command) => {
        if (command[1] === "has-session") return { stdout: "", exitCode: 0 };
        if (command[1] === "new-window") return { stdout: "@77 %1 4242\n", exitCode: 0 };
        return { stdout: "", exitCode: 0 };
      },
    });

    try {
      await processes.reconcileAdmission();

      expect(state.admission).toEqual({ cap: 1, active: [root], queue: [] });
      expect(state.trees[root]).toMatchObject({
        status: "active",
        locator: { tmuxSession: "legion-omp", tmuxWindowId: "@77" },
      });
      expect(errorLog).toHaveBeenCalledWith(expect.stringContaining(`demoted ${root}`));
    } finally {
      errorLog.mockRestore();
    }
  });

  it("reaps an unrecorded owner-marked window at boot, with no grace period, before re-spawning a demoted tree", async () => {
    const stateDir = await temporaryDir();
    const state = newLegionState("omp", 1);
    // Same crash as above, plus a leaked tmux window from that same prior
    // spawn attempt: nothing in state names it, so boot must not wait out
    // the periodic sweep's grace period to reap it.
    state.trees[root] = {
      root,
      generation: 1,
      status: "active",
      launchFailures: 0,
    };
    state.admission.active.push(root);
    state.issues[root] = { key: root, title: "Root", status: "todo", children: [] };
    const errorLog = vi.spyOn(console, "error").mockImplementation(() => {});
    const killedWindows: string[] = [];
    let newWindowCalls = 0;
    const { manager: processes } = manager(state, {
      config: config(stateDir),
      run: async (command) => {
        if (command[1] === "list-windows") {
          return {
            stdout: `@99\tlegion-omp\t${Date.parse("2026-08-24T00:00:00.000Z") / 1000}\n`,
            exitCode: 0,
          };
        }
        if (command[1] === "kill-window") {
          killedWindows.push(command[3] ?? "");
          return { stdout: "", exitCode: 0 };
        }
        if (command[1] === "has-session") return { stdout: "", exitCode: 0 };
        if (command[1] === "new-window") {
          newWindowCalls += 1;
          return { stdout: "@77 %1 4242\n", exitCode: 0 };
        }
        return { stdout: "", exitCode: 0 };
      },
    });

    try {
      await processes.reconcileAdmission();

      expect(killedWindows).toEqual(["@99"]);
      expect(newWindowCalls).toBe(1);
      expect(state.trees[root]).toMatchObject({
        status: "active",
        locator: { tmuxSession: "legion-omp", tmuxWindowId: "@77" },
      });
    } finally {
      errorLog.mockRestore();
    }
  });

  it("kills a just-created window and rolls back the launch when its ownership marker fails", async () => {
    const stateDir = await temporaryDir();
    const state = newLegionState("omp", 1);
    state.admission.active.push(root);
    const killedWindows: string[] = [];
    const { manager: processes } = manager(state, {
      config: config(stateDir),
      run: async (command) => {
        if (command[1] === "has-session") return { stdout: "", exitCode: 0 };
        if (command[1] === "new-window") return { stdout: "@88 %1 4242\n", exitCode: 0 };
        if (command[1] === "set-option" && command[2] === "-w") {
          return { stdout: "marker rejected", exitCode: 1 };
        }
        if (command[1] === "kill-window") {
          killedWindows.push(command[3] ?? "");
          return { stdout: "", exitCode: 0 };
        }
        return { stdout: "", exitCode: 0 };
      },
    });

    // Every window is either recorded (locator assigned) or reaped: the
    // marker never landed, so nothing would ever find this window again —
    // it must be killed synchronously instead of leaked.
    await expect(processes.spawnRoot(root)).rejects.toThrow("tmux window ownership marker failed");

    expect(killedWindows).toEqual(["@88"]);
    expect(state.trees[root]?.status).toBe("queued");
    expect(state.trees[root]?.locator).toBeUndefined();
  });

  it("leaves launch-failed trees queued when reconciling admission capacity", async () => {
    const stateDir = await temporaryDir();
    const state = newLegionState("omp", 1);
    tree(state);
    state.trees[root].status = "launch-failed";
    state.admission.queue.push(root);
    const { manager: processes } = manager(state, {
      config: config(stateDir, { admissionCap: 2 }),
    });

    await processes.reconcileAdmission();

    expect(state.admission).toEqual({ cap: 2, active: [], queue: [root] });
  });

  it("skips launch-failed entries while promoting eligible queued trees during admission reconciliation", async () => {
    const stateDir = await temporaryDir();
    const state = newLegionState("omp", 1);
    tree(state);
    state.trees[root].status = "launch-failed";
    state.admission.queue.push(root, child, grandchild);
    const { manager: processes } = manager(state, {
      config: config(stateDir, { admissionCap: 2 }),
    });

    await processes.reconcileAdmission();

    expect(state.admission).toEqual({ cap: 2, active: [child, grandchild], queue: [root] });
    expect(state.trees[root].status).toBe("launch-failed");
    expect(state.trees[child]?.status).toBe("active");
    expect(state.trees[grandchild]?.status).toBe("active");
  });

  it("persists a raised cap even when reconciliation has no queued trees to promote", async () => {
    const stateDir = await temporaryDir();
    const state = newLegionState("omp", 1);
    let saves = 0;
    const { manager: processes } = manager(state, {
      config: config(stateDir, { admissionCap: 3 }),
      saveState: async () => {
        saves += 1;
      },
    });

    await processes.reconcileAdmission();

    expect(state.admission).toEqual({ cap: 3, active: [], queue: [] });
    expect(saves).toBeGreaterThan(0);
  });

  it("attempts each queued tree exactly once when every boot-promoted launch fails", async () => {
    const stateDir = await temporaryDir();
    const state = newLegionState("omp", 1);
    state.admission.queue.push(root, child, grandchild);
    const settled = Promise.withResolvers<void>();
    const { manager: processes } = manager(state, {
      config: config(stateDir, { admissionCap: 2 }),
      saveState: async () => {
        const failures = [root, child, grandchild].reduce(
          (sum, issue) => sum + (state.trees[issue]?.launchFailures ?? 0),
          0
        );
        if (failures === 3) settled.resolve();
      },
      run: async (command) =>
        command[1] === "new-window"
          ? { stdout: "window creation failed", exitCode: 1 }
          : { stdout: "", exitCode: 0 },
    });

    await processes.reconcileAdmission();
    await settled.promise;
    await Promise.resolve();

    expect(state.admission.active).toEqual([]);
    expect([...state.admission.queue].sort()).toEqual([root, child, grandchild].sort());
    expect(state.trees[root]?.launchFailures).toBe(1);
    expect(state.trees[child]?.launchFailures).toBe(1);
    expect(state.trees[grandchild]?.launchFailures).toBe(1);
  });

  it("never promotes stale queue entries whose trees are not queued", async () => {
    const stateDir = await temporaryDir();
    const state = newLegionState("omp", 1);
    const closed = "LEGION-45";
    tree(state, root);
    tree(state, child);
    tree(state, grandchild);
    tree(state, closed);
    state.trees[root].status = "active";
    state.trees[child].status = "lingering";
    state.trees[grandchild].status = "dead";
    state.trees[closed].status = "closed";
    state.admission.active.push(root);
    state.admission.queue.push(root, child, grandchild, closed);
    const { manager: processes, commands } = manager(state, {
      config: config(stateDir, { admissionCap: 5 }),
    });

    await processes.reconcileAdmission();

    expect(state.admission).toEqual({
      cap: 5,
      active: [root],
      queue: [root, child, grandchild, closed],
    });
    expect(commands).toEqual([
      [
        "tmux",
        "list-windows",
        "-t",
        "legion-omp",
        "-F",
        "#{window_id}\t#{@legion_owner}\t#{window_activity}",
      ],
      [
        "tmux",
        "list-panes",
        "-a",
        "-F",
        "#{pane_id}\t#{window_id}\t#{@legion_owner}\t#{pane_start_command}\t#{pane_activity}",
      ],
    ]);
  });

  it("marks a tree launch-failed after its third launch failure and publishes a controller anomaly", async () => {
    const stateDir = await temporaryDir();
    const state = newLegionState("omp", 1);
    state.admission.active.push(root);
    const { manager: processes, publications } = manager(state, {
      config: config(stateDir),
      run: async (command) =>
        command[1] === "new-window"
          ? { stdout: "window creation failed", exitCode: 1 }
          : { stdout: "", exitCode: 0 },
    });

    await expect(processes.spawnRoot(root)).rejects.toThrow("tmux new-window failed");
    await expect(processes.spawnRoot(root)).rejects.toThrow("tmux new-window failed");
    await expect(processes.spawnRoot(root)).rejects.toThrow("tmux new-window failed");

    expect(state.trees[root]).toMatchObject({
      status: "launch-failed",
      launchFailures: 3,
    });
    expect(state.admission).toEqual({ cap: 1, active: [], queue: [] });
    expect(publications).toEqual([
      {
        subject: `notifications.role.${controllerToken("omp")}`,
        json: JSON.stringify({
          type: "launch-failed",
          issue: root,
          failures: 3,
        }),
      },
    ]);
  });

  it("kills the just-spawned window before propagating a saveState failure after a successful spawn, without rolling back the launch or requeuing it as a failure", async () => {
    const stateDir = await temporaryDir();
    const state = newLegionState("omp", 1);
    state.admission.active.push(root);
    state.issues[root] = { key: root, title: "Root", status: "todo", children: [] };
    state.trees[root] = { root, generation: 0, status: "active", launchFailures: 0 };
    const killedWindows: string[] = [];
    const { manager: processes } = manager(state, {
      config: config(stateDir),
      run: async (command) => {
        if (command[1] === "has-session") return { stdout: "", exitCode: 0 };
        if (command[1] === "new-window") return { stdout: "@42 %1 4242\n", exitCode: 0 };
        if (command[1] === "kill-window") {
          killedWindows.push(command[3] ?? "");
          return { stdout: "", exitCode: 0 };
        }
        return { stdout: "", exitCode: 0 };
      },
      saveState: async () => {
        throw new Error("disk full");
      },
    });

    // The spawn itself (tmux window, locator, generation) already
    // succeeded before this save runs — only persisting that fact failed.
    // Treating this like a launch failure would roll back the tracked
    // locator and requeue the tree while a real window keeps running, so
    // instead the window is killed directly and the failure propagates
    // distinctly (see `SpawnPersistenceFailure`).
    await expect(processes.spawnRoot(root)).rejects.toThrow("disk full");

    expect(killedWindows).toEqual(["@42"]);
    expect(state.trees[root]).toMatchObject({
      generation: 1,
      status: "active",
      launchFailures: 0,
      locator: { tmuxSession: "legion-omp", tmuxWindowId: "@42" },
    });
    expect(state.admission).toEqual({ cap: 1, active: [root], queue: [] });
  });

  it("clears a launch-failed tree's counter when controller admission retries it", async () => {
    const stateDir = await temporaryDir();
    const state = newLegionState("omp", 1);
    state.trees[root] = {
      root,
      generation: 3,
      status: "launch-failed",
      launchFailures: 3,
    };
    const { manager: processes } = manager(state, {
      config: config(stateDir),
      run: async () => ({ stdout: "", exitCode: 0 }),
    });

    expect(processes.admit(root)).toBe("queued");

    expect(state.trees[root]).toMatchObject({
      status: "active",
      launchFailures: 0,
    });
  });

  it("schedules a queued failed tree immediately when controller admission retries it", async () => {
    const stateDir = await temporaryDir();
    const state = newLegionState("omp", 1);
    state.trees[root] = {
      root,
      generation: 1,
      status: "queued",
      launchFailures: 1,
    };
    state.admission.queue.push(root);
    const { manager: processes } = manager(state, {
      config: config(stateDir),
      run: async () => ({ stdout: "", exitCode: 0 }),
    });

    expect(processes.admit(root)).toBe("queued");

    expect(state.admission).toEqual({ cap: 1, active: [root], queue: [] });
    expect(state.trees[root]).toMatchObject({
      status: "active",
      launchFailures: 1,
    });
  });

  it("serializes concurrent resurrection attempts for the same dead generation", async () => {
    const stateDir = await temporaryDir();
    const sessionFile = path.join(stateDir, "architect-session.json");
    await writeFile(sessionFile, "{}", "utf8");
    const state = newLegionState("omp", 1);
    tree(state);
    const locator = state.trees[root].locator;
    if (!locator) throw new Error("test root is missing a locator");
    state.trees[root].locator = { ...locator, ompSessionFile: sessionFile };
    let windows = 0;
    const { manager: processes } = manager(state, {
      config: config(stateDir),
      run: async (command) => {
        if (command[1] === "list-windows") return { stdout: "", exitCode: 1 };
        if (command[1] === "has-session") return { stdout: "", exitCode: 0 };
        if (command[1] === "new-window") {
          windows += 1;
          return { stdout: "@42 %1 12345\n", exitCode: 0 };
        }
        if (command[1] === "list-panes" && command.includes("#{pane_id}")) {
          return windows > 0 ? { stdout: "%1\n", exitCode: 0 } : { stdout: "", exitCode: 1 };
        }
        if (command[1] === "list-panes" && command.includes("#{pane_pid}")) {
          return windows > 0 ? { stdout: "12345\n", exitCode: 0 } : { stdout: "", exitCode: 1 };
        }
        return { stdout: "", exitCode: 0 };
      },
    });

    await Promise.all([processes.resurrect(root), processes.resurrect(root)]);

    expect(windows).toBe(1);
    expect(state.trees[root].generation).toBe(2);
  });

  it("resumes the recorded OMP session when resurrecting a dead root", async () => {
    const stateDir = await temporaryDir();
    const sessionFile = path.join(stateDir, "architect-session.json");
    await writeFile(sessionFile, "{}", "utf8");
    const state = newLegionState("omp", 1);
    tree(state);
    const locator = state.trees[root].locator;
    if (!locator) throw new Error("test root is missing a locator");
    state.trees[root].locator = { ...locator, ompSessionFile: sessionFile };
    const { manager: processes, commands } = manager(state, {
      config: config(stateDir),
    });

    await processes.resurrect(root);

    const workspace = path.join(stateDir, "workspaces", "sjawhar", "legion", "legion-42");
    const extension = path.resolve(import.meta.dir, "../../../../pi-envoy");
    const entrypoint = path.resolve(import.meta.dir, "../../cli/index.ts");
    const socketPath = path.join(stateDir, "workers", "architect-9e2fb104.sock");
    const launch = commands.find((command) => command[0] === "tmux" && command[1] === "new-window");
    expect(launch?.at(-1)).toBe(
      `cd ${workspace} && ${process.execPath} ${entrypoint} worker-shim --socket ${socketPath} -- /opt/oh-my-pi/18.0.3/omp --resume=${sessionFile} --mode rpc --append-system-prompt "$(cat ${extension}/roles/architect-root.md)" --append-system-prompt '${addressingFragment("omp", root, root, "architect").replaceAll("'", "'\\''")}'`
    );
  });

  it("starts fresh when launching a root outside the resurrection path", async () => {
    const stateDir = await temporaryDir();
    const sessionFile = path.join(stateDir, "architect-session.json");
    await writeFile(sessionFile, "{}", "utf8");
    const state = newLegionState("omp", 1);
    tree(state);
    const locator = state.trees[root].locator;
    if (!locator) throw new Error("test root is missing a locator");
    state.trees[root].locator = { ...locator, ompSessionFile: sessionFile };
    const log = vi.spyOn(console, "info").mockImplementation(() => {});
    const { manager: processes, commands } = manager(state, {
      config: config(stateDir),
    });

    try {
      await processes.spawnRoot(root);
      expect(log).not.toHaveBeenCalled();
    } finally {
      log.mockRestore();
    }

    const workspace = path.join(stateDir, "workspaces", "sjawhar", "legion", "legion-42");
    const extension = path.resolve(import.meta.dir, "../../../../pi-envoy");
    const entrypoint = path.resolve(import.meta.dir, "../../cli/index.ts");
    const socketPath = path.join(stateDir, "workers", "architect-9e2fb104.sock");
    const launch = commands.find((command) => command[0] === "tmux" && command[1] === "new-window");
    expect(launch?.at(-1)).toBe(
      `cd ${workspace} && ${process.execPath} ${entrypoint} worker-shim --socket ${socketPath} -- /opt/oh-my-pi/18.0.3/omp --mode rpc --append-system-prompt "$(cat ${extension}/roles/architect-root.md)" --append-system-prompt '${addressingFragment("omp", root, root, "architect").replaceAll("'", "'\\''")}'`
    );
  });

  it("fails a resurrection loudly when the recorded OMP session file is missing, never starting fresh", async () => {
    const stateDir = await temporaryDir();
    const sessionFile = path.join(stateDir, "missing-architect-session.json");
    const state = newLegionState("omp", 1);
    tree(state);
    const locator = state.trees[root].locator;
    if (!locator) throw new Error("test root is missing a locator");
    state.trees[root].locator = { ...locator, ompSessionFile: sessionFile };
    const { manager: processes, commands } = manager(state, {
      config: config(stateDir),
    });

    await expect(processes.resurrect(root)).rejects.toThrow(/recorded OMP session file is missing/);

    expect(
      commands.some((command) => command[0] === "tmux" && command[1] === "new-window")
    ).toBeFalse();
    expect(state.trees[root].launchFailures).toBe(1);
    expect(state.trees[root].status).toBe("queued");
  });

  it("clears completed-tree phases and releases its admission slot at linger start, then shuts down its recorded tmux tree", async () => {
    const state = newLegionState("omp", 1);
    tree(state);
    state.admission.active.push(root);
    state.roles[roleToken("omp", root, "architect")] = {
      issue: root,
      role: "architect",
    };
    state.issues[child] = {
      key: child,
      title: "Child",
      status: "done",
      parent: root,
      children: [],
    };
    state.phases[root] = { phase: "merger", sessionId: "ses_root_merger" };
    state.phases[child] = { phase: "reviewer", sessionId: "ses_child_reviewer" };
    const { manager: processes, commands, publications } = manager(state);

    await processes.beginLinger(root);

    expect(state.trees[root]).toMatchObject({
      status: "lingering",
      lingerUntil: "2026-08-24T02:00:00.000Z",
    });
    expect(state.admission).toEqual({ cap: 1, active: [], queue: [] });
    expect(state.phases[root]).toBeUndefined();
    expect(state.phases[child]).toBeUndefined();

    await processes.expireLinger(root);

    expect(state.trees[root].status).toBe("closed");
    expect(state.roles[roleToken("omp", root, "architect")]).toBeUndefined();
    expect(state.phases[root]).toBeUndefined();
    expect(state.phases[child]).toBeUndefined();
    expect(publications).toEqual([]);
    // The default fixture's fake pane never reports a live pid, so `probe` sees the root as
    // already dead and `stopProcess` skips straight to reaping its recorded pane.
    expect(commands).toContainEqual(["tmux", "kill-pane", "-t", "%0"]);
  });

  it("awaits a promoted queued tree's full spawn attempt before beginLinger resolves", async () => {
    const stateDir = await temporaryDir();
    const state = newLegionState("omp", 1);
    tree(state);
    state.admission.active.push(root);
    state.admission.queue.push(child);
    state.issues[child] = { key: child, title: "Child", status: "todo", children: [] };
    const { manager: processes } = manager(state, {
      config: config(stateDir),
      run: async (command) => {
        if (command[1] === "has-session") return { stdout: "", exitCode: 0 };
        if (command[1] === "new-window") return { stdout: "@99 %1 4242\n", exitCode: 0 };
        return { stdout: "", exitCode: 0 };
      },
    });

    // The whole release-promote-spawn cascade releaseSlot triggers must
    // settle before beginLinger's own promise resolves: otherwise the
    // durable transaction's outer save (applyDurableEvent) could persist
    // child as "active" before its spawn recorded a locator, and a crash
    // in that window would leave it consuming a slot with no tmux window
    // forever (reconcileAdmission only promotes queued work at boot, it
    // never resurrects an already-active tree with no locator).
    await processes.beginLinger(root);

    expect(state.admission).toEqual({ cap: 1, active: [child], queue: [] });
    expect(state.trees[child]).toMatchObject({
      status: "active",
      locator: { tmuxSession: "legion-omp", tmuxWindowId: "@99" },
    });
  });

  it("propagates a promoted spawn's persistence failure out of beginLinger instead of swallowing it in startRoot", async () => {
    const stateDir = await temporaryDir();
    const state = newLegionState("omp", 1);
    tree(state);
    state.admission.active.push(root);
    state.admission.queue.push(child);
    state.issues[child] = { key: child, title: "Child", status: "todo", children: [] };
    let saveCalls = 0;
    const { manager: processes } = manager(state, {
      config: config(stateDir),
      run: async (command) => {
        if (command[1] === "has-session") return { stdout: "", exitCode: 0 };
        if (command[1] === "new-window") return { stdout: "@99 %1 4242\n", exitCode: 0 };
        return { stdout: "", exitCode: 0 };
      },
      saveState: async () => {
        saveCalls += 1;
        // The first save (child promoted into active, spliced from queue)
        // must succeed; the second — spawnRoot's own save after its
        // successful spawn — is the one under test.
        if (saveCalls === 2) throw new Error("disk full");
      },
    });

    // A SpawnPersistenceFailure must never be treated as a launch failure
    // by startRoot (which would roll back the just-created tmux window
    // and requeue child) — it must propagate all the way out of
    // beginLinger, so the durable transaction dispatching this linger
    // effect fails and goes fatal, exactly like any other durable effect
    // whose post-mutation save fails.
    await expect(processes.beginLinger(root)).rejects.toThrow("disk full");

    expect(state.trees[child]).toMatchObject({
      status: "active",
      launchFailures: 0,
      locator: { tmuxSession: "legion-omp", tmuxWindowId: "@99" },
    });
    expect(state.admission.active).toEqual([child]);
    expect(state.admission.queue).toEqual([]);
  });
  it("gracefully stops the root and every worker under the tree via their own shim sockets before removing their claims", async () => {
    const state = newLegionState("omp", 1);
    tree(state);
    if (!state.trees[root]?.locator) throw new Error("test root is missing a locator");
    state.trees[root].locator = {
      ...state.trees[root].locator,
      tmuxPaneId: "%0",
      socketPath: "/state/workers/architect.sock",
    };
    state.issues[child] = {
      key: child,
      title: "Child",
      status: "in_progress",
      parent: root,
      children: [],
    };
    state.roles[roleToken("omp", child, "implementer")] = {
      issue: child,
      role: "implementer",
      locator: {
        tmuxSession: "legion-omp",
        tmuxWindowId: "@99",
        tmuxPaneId: "%1",
        socketPath: "/state/workers/implementer.sock",
      },
    };
    const shutdownCalls: string[] = [];
    const commands: string[][] = [];
    const { manager: processes } = manager(state, {
      // `probe(root)` must find the recorded root pane alive so `closeTree`'s unilateral
      // (non-self-report) leg actually attempts the root's own graceful stop instead of
      // skipping straight to a kill on the assumption nothing is there.
      run: async (command) => {
        commands.push(command);
        if (
          command[0] === "tmux" &&
          command[1] === "list-panes" &&
          command.includes("#{pane_pid}")
        ) {
          return { stdout: "4242\n", exitCode: 0 };
        }
        return { stdout: "", exitCode: 0 };
      },
      connectWorkerRpc: async (socketPath) => {
        const client = fakeWorkerRpcClient();
        const shutdown = client.shutdown.bind(client);
        client.shutdown = () => {
          shutdownCalls.push(socketPath);
          shutdown();
        };
        return client;
      },
    });

    await processes.closeTree(root);

    expect(shutdownCalls.sort()).toEqual([
      "/state/workers/architect.sock",
      "/state/workers/implementer.sock",
    ]);
    expect(commands.filter((command) => command[1] === "kill-window")).toEqual([]);
    expect(commands.filter((command) => command[1] === "kill-pane")).toEqual([]);
    expect(state.roles[roleToken("omp", child, "implementer")]).toBeUndefined();
    expect(state.trees[root].locator).toBeUndefined();
    expect(state.trees[root].status).toBe("closed");
  });

  it("skips gracefully stopping the root's own process on a self-report, but still gracefully stops every worker", async () => {
    // markProcessDead's doc comment explains why: the architect's own `session_shutdown` hook
    // awaits `/process/exit` before OMP exits, so a self-reported closeTree IS that same
    // still-running process — asking its own shim to close would deadlock forever waiting on
    // itself. This is the one case where `probe`'s "alive" would otherwise be true but the root
    // leg must be skipped unconditionally.
    const state = newLegionState("omp", 1);
    tree(state);
    if (!state.trees[root]?.locator) throw new Error("test root is missing a locator");
    state.trees[root].locator = {
      ...state.trees[root].locator,
      tmuxPaneId: "%0",
      socketPath: "/state/workers/architect.sock",
    };
    state.issues[child] = {
      key: child,
      title: "Child",
      status: "in_progress",
      parent: root,
      children: [],
    };
    state.roles[roleToken("omp", child, "implementer")] = {
      issue: child,
      role: "implementer",
      locator: {
        tmuxSession: "legion-omp",
        tmuxWindowId: "@99",
        tmuxPaneId: "%1",
        socketPath: "/state/workers/implementer.sock",
      },
    };
    const connectedSockets: string[] = [];
    const commands: string[][] = [];
    const { manager: processes } = manager(state, {
      run: async (command) => {
        commands.push(command);
        if (
          command[0] === "tmux" &&
          command[1] === "list-panes" &&
          command.includes("#{pane_pid}")
        ) {
          return { stdout: "4242\n", exitCode: 0 };
        }
        return { stdout: "", exitCode: 0 };
      },
      connectWorkerRpc: async (socketPath) => {
        connectedSockets.push(socketPath);
        return fakeWorkerRpcClient();
      },
    });

    await processes.closeTree(root, { stopRoot: false });

    expect(connectedSockets).toEqual(["/state/workers/implementer.sock"]);
    expect(commands.filter((command) => command[1] === "kill-window")).toEqual([]);
    expect(commands.filter((command) => command[1] === "kill-pane")).toEqual([]);
    expect(state.trees[root].locator).toBeUndefined();
    expect(state.trees[root].status).toBe("closed");
  });

  it("kills only a timed-out worker's own pane on a tree close, leaving a sibling that closed gracefully untouched", async () => {
    const state = newLegionState("omp", 1);
    tree(state);
    if (!state.trees[root]?.locator) throw new Error("test root is missing a locator");
    state.trees[root].locator = {
      ...state.trees[root].locator,
      tmuxPaneId: "%0",
      socketPath: "/state/workers/architect.sock",
    };
    state.issues[child] = {
      key: child,
      title: "Child",
      status: "in_progress",
      parent: root,
      children: [],
    };
    state.issues[grandchild] = {
      key: grandchild,
      title: "Grandchild",
      status: "in_progress",
      parent: root,
      children: [],
    };
    state.roles[roleToken("omp", child, "implementer")] = {
      issue: child,
      role: "implementer",
      locator: {
        tmuxSession: "legion-omp",
        tmuxWindowId: "@99",
        tmuxPaneId: "%1",
        socketPath: "/state/workers/hung.sock",
      },
    };
    state.roles[roleToken("omp", grandchild, "tester")] = {
      issue: grandchild,
      role: "tester",
      locator: {
        tmuxSession: "legion-omp",
        tmuxWindowId: "@100",
        tmuxPaneId: "%2",
        socketPath: "/state/workers/graceful.sock",
      },
    };
    const commands: string[][] = [];
    const { manager: processes } = manager(state, {
      // A real macrotask (not an instantly-resolving microtask) for the timeout: microtasks
      // (including the default fake client's queueMicrotask-deferred close) always fully drain
      // before a timer fires, so the graceful worker's close deterministically wins this race
      // without relying on engine-specific microtask-tick counting.
      sleep: async () => new Promise<void>((resolve) => setTimeout(resolve, 0)),
      run: async (command) => {
        commands.push(command);
        if (
          command[0] === "tmux" &&
          command[1] === "list-panes" &&
          command.includes("#{pane_pid}")
        ) {
          return { stdout: "4242\n", exitCode: 0 };
        }
        return { stdout: "", exitCode: 0 };
      },
      connectWorkerRpc: async (socketPath) => {
        if (socketPath === "/state/workers/hung.sock") {
          const stuck = fakeWorkerRpcClient();
          stuck.shutdown = () => {};
          return stuck;
        }
        return fakeWorkerRpcClient();
      },
    });

    await processes.closeTree(root);

    expect(commands).toContainEqual(["tmux", "kill-pane", "-t", "%1"]);
    expect(commands).not.toContainEqual(["tmux", "kill-pane", "-t", "%2"]);
    expect(commands).not.toContainEqual(["tmux", "kill-pane", "-t", "%0"]);
    expect(commands.filter((command) => command[1] === "kill-window")).toEqual([]);
  });

  it("throws StopFailed and leaves the tree lingering (not closed) when a root locator is a corrupt record missing a pane id", async () => {
    // `TmuxWindowLocator.tmuxPaneId` (the root's own locator type) is optional — unlike the
    // strictly-required `WorkerLocator.tmuxPaneId` every worker claim carries — so this is a
    // real, type-reachable state for a root: a launch that recorded only a window id before a
    // pane id was ever confirmed, or a pre-pane-id legacy record.
    const state = newLegionState("omp", 1);
    tree(state);
    if (!state.trees[root]?.locator) throw new Error("test root is missing a locator");
    state.trees[root].locator = {
      tmuxSession: "legion-omp",
      tmuxWindowId: "@42",
      // No tmuxPaneId.
      socketPath: "/state/workers/architect.sock",
    };
    const errorLog = vi.spyOn(console, "error").mockImplementation(() => {});
    const commands: string[][] = [];
    const { manager: processes } = manager(state, {
      sleep: async () => {},
      run: async (command) => {
        commands.push(command);
        if (
          command[0] === "tmux" &&
          command[1] === "list-panes" &&
          command.includes("#{pane_pid}")
        ) {
          return { stdout: "4242\n", exitCode: 0 };
        }
        return { stdout: "", exitCode: 0 };
      },
      connectWorkerRpc: async () => {
        const stuck = fakeWorkerRpcClient();
        stuck.shutdown = () => {};
        return stuck;
      },
    });

    await expect(processes.closeTree(root)).rejects.toThrow(StopFailed);

    expect(commands.filter((command) => command[1] === "kill-pane")).toEqual([]);
    expect(commands.filter((command) => command[1] === "kill-window")).toEqual([]);
    expect(errorLog).toHaveBeenCalledWith(
      expect.stringContaining(`root process failed to stop while closing ${root}`),
      expect.objectContaining({ message: expect.stringContaining("missing a pane id") })
    );
    // Never marked closed while a possibly-live process's locator couldn't be confirmed
    // stopped: the tree is left lingering (with a fresh retry deadline) for the sweep.
    expect(state.trees[root].status).toBe("lingering");
    expect(state.trees[root].locator).toEqual({
      tmuxSession: "legion-omp",
      tmuxWindowId: "@42",
      socketPath: "/state/workers/architect.sock",
    });
    errorLog.mockRestore();
  });

  it("tolerates killing a pane tmux already reaped without logging it as a failure", async () => {
    const state = newLegionState("omp", 1);
    tree(state);
    if (!state.trees[root]?.locator) throw new Error("test root is missing a locator");
    state.trees[root].locator = {
      ...state.trees[root].locator,
      tmuxPaneId: "%0",
      socketPath: "/state/workers/architect.sock",
    };
    const errorLog = vi.spyOn(console, "error").mockImplementation(() => {});
    const { manager: processes } = manager(state, {
      sleep: async () => {},
      run: async (command) => {
        if (
          command[0] === "tmux" &&
          command[1] === "list-panes" &&
          command.includes("#{pane_pid}")
        ) {
          return { stdout: "4242\n", exitCode: 0 };
        }
        if (command[0] === "tmux" && command[1] === "kill-pane") {
          return { stdout: "", stderr: "can't find pane: %0", exitCode: 1 };
        }
        return { stdout: "", exitCode: 0 };
      },
      connectWorkerRpc: async () => {
        const stuck = fakeWorkerRpcClient();
        stuck.shutdown = () => {};
        return stuck;
      },
    });

    await processes.closeTree(root);

    expect(errorLog).not.toHaveBeenCalled();
    errorLog.mockRestore();
  });

  it("is idempotent: a second concurrent closeTree call awaits the same in-flight close instead of stopping each locator twice", async () => {
    const state = newLegionState("omp", 1);
    tree(state);
    if (!state.trees[root]?.locator) throw new Error("test root is missing a locator");
    state.trees[root].locator = {
      ...state.trees[root].locator,
      tmuxPaneId: "%0",
      socketPath: "/state/workers/architect.sock",
    };
    const shutdownCalls: string[] = [];
    const { manager: processes } = manager(state, {
      run: async (command) => {
        if (
          command[0] === "tmux" &&
          command[1] === "list-panes" &&
          command.includes("#{pane_pid}")
        ) {
          return { stdout: "4242\n", exitCode: 0 };
        }
        return { stdout: "", exitCode: 0 };
      },
      connectWorkerRpc: async (socketPath) => {
        const client = fakeWorkerRpcClient();
        const shutdown = client.shutdown.bind(client);
        client.shutdown = () => {
          shutdownCalls.push(socketPath);
          shutdown();
        };
        return client;
      },
    });

    const [first, second] = await Promise.all([
      processes.closeTree(root),
      processes.closeTree(root),
    ]);

    expect(first).toBeUndefined();
    expect(second).toBeUndefined();
    expect(shutdownCalls).toEqual(["/state/workers/architect.sock"]);
    expect(state.trees[root].status).toBe("closed");
  });

  it("never joins an in-flight closeTree from a root's own self-report, letting the outer close complete once the root actually exits", async () => {
    // The self-report deadlock: closeTree's root leg sends `{type:"shutdown"}` and awaits the
    // root's own shim socket closing. The root's real `session_shutdown` hook awaits
    // `/process/exit` (== `reportRootExit`) before OMP finishes exiting -- so if
    // `reportRootExit` joined this same in-flight close, the close would wait on the root
    // exiting, and the root's own exit would wait on this call returning: deadlock. This test's
    // fake root client simulates exactly that self-report, firing synchronously from inside
    // `shutdown()`, before its own `closed` promise ever resolves.
    const state = newLegionState("omp", 1);
    tree(state);
    if (!state.trees[root]?.locator) throw new Error("test root is missing a locator");
    state.trees[root].locator = {
      ...state.trees[root].locator,
      tmuxPaneId: "%0",
      socketPath: "/state/workers/architect.sock",
    };
    const commands: string[][] = [];
    let processes!: ProcessManager;
    let reportRootExitSettled = false;
    const rootClient = fakeWorkerRpcClient();
    rootClient.shutdown = () => {
      void processes.reportRootExit(root).then(() => {
        reportRootExitSettled = true;
        // Only now does OMP actually finish exiting session_shutdown -- the shim's socket closes.
        rootClient.close();
      });
    };
    ({ manager: processes } = manager(state, {
      run: async (command) => {
        commands.push(command);
        if (
          command[0] === "tmux" &&
          command[1] === "list-panes" &&
          command.includes("#{pane_pid}")
        ) {
          return { stdout: "4242\n", exitCode: 0 };
        }
        return { stdout: "", exitCode: 0 };
      },
      connectWorkerRpc: async () => rootClient,
    }));

    const closing = processes.closeTree(root);

    for (let attempt = 0; attempt < 100 && !reportRootExitSettled; attempt++) {
      await new Promise((resolve) => setTimeout(resolve, 1));
    }
    expect(reportRootExitSettled).toBe(true);

    await closing;

    expect(state.trees[root].status).toBe("closed");
    expect(commands.filter((command) => command[1] === "kill-pane")).toEqual([]);
  });

  it("marks a tree lingering as its first durable act before any stop, so a crash mid-close leaves it retryable by the sweep instead of stuck active", async () => {
    const stateDir = await temporaryDir();
    const stateFile = path.join(stateDir, "state.json");
    const state = newLegionState("omp", 1);
    tree(state);
    if (!state.trees[root]?.locator) throw new Error("test root is missing a locator");
    state.trees[root].locator = {
      ...state.trees[root].locator,
      tmuxPaneId: "%0",
      socketPath: "/state/workers/architect.sock",
    };
    expect(state.trees[root].status).toBe("active");
    let saveCalls = 0;
    const { manager: processes } = manager(state, {
      saveState: async () => {
        saveCalls += 1;
        // Call 1 is closeTree's own upfront lingering mark: let it durably land on disk. Call 2
        // is the close's final success save recording "closed" -- simulate a crash losing that
        // write entirely (never reaches disk), not merely rejecting after writing.
        if (saveCalls === 2) throw new Error("simulated crash mid-close");
        await legionStateSaveState(stateFile, state);
      },
      run: async (command) => {
        if (
          command[0] === "tmux" &&
          command[1] === "list-panes" &&
          command.includes("#{pane_pid}")
        ) {
          return { stdout: "4242\n", exitCode: 0 };
        }
        return { stdout: "", exitCode: 0 };
      },
      connectWorkerRpc: async () => fakeWorkerRpcClient(),
    });

    await expect(processes.closeTree(root)).rejects.toThrow("simulated crash mid-close");
    expect(saveCalls).toBe(2);

    // What actually survives the "crash" is whatever the last successful write put on disk --
    // never the in-memory object, which a real process crash would discard entirely.
    const reloaded = await legionStateLoadState(stateFile, { project: "omp", cap: 1 });
    expect(reloaded.trees[root]?.status).toBe("lingering");
    expect(reloaded.trees[root]?.lingerUntil).toBeString();

    // The periodic sweep's retry, against the reloaded (post-crash) state: finishes cleanly.
    const { manager: retryProcesses } = manager(reloaded, {
      saveState: () => legionStateSaveState(stateFile, reloaded),
      run: async (command) => {
        if (
          command[0] === "tmux" &&
          command[1] === "list-panes" &&
          command.includes("#{pane_pid}")
        ) {
          return { stdout: "", exitCode: 1 };
        }
        return { stdout: "", exitCode: 0 };
      },
    });
    const retry = await retryProcesses.closeTree(root);
    expect(retry).toBeUndefined();
    expect(reloaded.trees[root]?.status).toBe("closed");
  });

  it("treats a socket error while waiting for a graceful close as unconfirmed, falling through to the kill instead of a false success", async () => {
    const state = newLegionState("omp", 1);
    tree(state);
    if (!state.trees[root]?.locator) throw new Error("test root is missing a locator");
    state.trees[root].locator = {
      ...state.trees[root].locator,
      tmuxPaneId: "%0",
      socketPath: "/state/workers/architect.sock",
    };
    const errorLog = vi.spyOn(console, "error").mockImplementation(() => {});
    const { manager: processes } = manager(state, {
      sleep: async () => {},
      run: async (command) => {
        if (
          command[0] === "tmux" &&
          command[1] === "list-panes" &&
          command.includes("#{pane_pid}")
        ) {
          return { stdout: "4242\n", exitCode: 0 };
        }
        if (command[0] === "tmux" && command[1] === "kill-pane") {
          return { stdout: "", stderr: "no server running", exitCode: 1 };
        }
        return { stdout: "", exitCode: 0 };
      },
      connectWorkerRpc: async () => {
        const closed = Promise.withResolvers<void>();
        return {
          closed: closed.promise,
          runState: "unknown" as const,
          negotiate: async () => {},
          prompt: async () => {},
          getState: async () => ({}),
          // A socket reset while waiting, not a graceful close -- must never be mistaken for
          // proof the process exited.
          shutdown: () => {
            queueMicrotask(() => closed.reject(new Error("socket reset")));
          },
          close: () => {},
          onIdle: () => {},
        };
      },
    });

    await expect(processes.closeTree(root)).rejects.toThrow(StopFailed);

    expect(state.trees[root].status).toBe("lingering");
    expect(state.trees[root].locator).toBeDefined();
    errorLog.mockRestore();
  });

  it("does not launch a replacement when retiring a dead-socket claim's pane fails to stop, surfacing StopFailed instead", async () => {
    const stateDir = await temporaryDir();
    const workspace = path.join(stateDir, "workspaces", "sjawhar", "legion", "issue-42");
    await mkdir(workspace, { recursive: true });
    const state = newLegionState("omp", 1);
    state.issues[root] = {
      key: root,
      title: "Root",
      status: "in_progress",
      children: [],
    };
    state.trees[root] = {
      root,
      generation: 1,
      status: "active",
      launchFailures: 0,
    };
    const token = roleToken("omp", root, "tester");
    state.roles[token] = {
      issue: root,
      role: "tester",
      generation: 1,
      sessionId: "ses_tester",
      locator: {
        tmuxSession: "legion-omp",
        tmuxWindowId: "@42",
        tmuxPaneId: "%7",
        socketPath: "/state/workers/dead-tester.sock",
      },
    };
    const commands: string[][] = [];
    const { manager: processes, state: managedState } = manager(state, {
      config: config(stateDir),
      sleep: async () => {},
      // A genuinely dead socket (the shim itself is gone), not merely a busy one that answers
      // `get_state` late — a connected-but-slow client now queues instead of retiring (see
      // `spawnWorker`'s liveness dialect), so this test's "does not launch a replacement, sees
      // StopFailed instead" scenario needs the actual dead-socket path: a connect failure.
      connectWorkerRpc: async () => {
        throw new Error("ECONNREFUSED");
      },
      run: async (command) => {
        commands.push(command);
        if (command[0] === "tmux" && command[1] === "kill-pane") {
          return { stdout: "", stderr: "no server running", exitCode: 1 };
        }
        return { stdout: "", exitCode: 0 };
      },
    });

    const error = await processes
      .spawnWorker(root, root, "tester", "verify again")
      .catch((caught: unknown) => caught);

    expect(error).toBeInstanceOf(StopFailed);
    // Never opened a replacement pane over a possibly-still-live one.
    expect(commands.filter((c) => c[1] === "new-window" || c[1] === "split-window")).toEqual([]);
    const claim = managedState.roles[token];
    if (!claim || !("issue" in claim)) throw new Error("worker claim disappeared");
    // The failed-to-stop claim's own locator is untouched -- it is the only durable handle left
    // on a pane that might still be alive.
    expect(claim.locator?.tmuxPaneId).toBe("%7");
  });

  it("revokes the root architect's and every worker's session capability when closing a tree", async () => {
    const state = newLegionState("omp", 1);
    tree(state);
    state.issues[child] = {
      key: child,
      title: "Child",
      status: "in_progress",
      parent: root,
      children: [],
    };
    const architectToken = roleToken("omp", root, "architect");
    state.roles[architectToken] = {
      issue: root,
      role: "architect",
      sessionId: "ses_architect",
    };
    const implementerToken = roleToken("omp", child, "implementer");
    state.roles[implementerToken] = {
      issue: child,
      role: "implementer",
      sessionId: "ses_implementer",
      locator: {
        tmuxSession: "legion-omp",
        tmuxWindowId: "@99",
        tmuxPaneId: "%1",
        socketPath: "/state/workers/implementer.sock",
      },
    };
    const { manager: processes, revokedSessions } = manager(state);

    await processes.closeTree(root);

    expect(revokedSessions).toContain("ses_architect");
    expect(revokedSessions).toContain("ses_implementer");
  });

  it("requests control directives on the sanitized tree generation topic", async () => {
    const state = newLegionState("omp", 1);
    tree(state, root, 3);
    const { manager: processes, controlRequests, publications } = manager(state);
    const original = { topic: "notifications.role.legion-omp-1", payload: "{}", eventId: "evt-1" };
    const directive: ControlDirective = {
      type: "reclaim-architect",
      issue: root,
      redeliver: original,
    };

    await processes.controlDirective(root, directive, false);

    expect(controlRequests).toEqual([
      {
        subject: "legion.ctl.legion-42.3",
        json: JSON.stringify(directive),
      },
    ]);
    expect(publications).toEqual([]);
  });

  it("resumes a live worker directly (its shim socket answers) with a state-derived catch-up, never the raw missed event", async () => {
    const state = newLegionState("omp", 1);
    tree(state);
    state.issues[child] = {
      key: child,
      title: "Child",
      status: "in_progress",
      parent: root,
      children: [],
    };
    const role: LegionRole = "implementer";
    const token = roleToken("omp", child, role);
    state.roles[token] = {
      issue: child,
      role,
      sessionId: "ses_implementer",
      generation: 1,
      locator: {
        tmuxSession: "legion-omp",
        tmuxWindowId: "@42",
        tmuxPaneId: "%2",
        socketPath: "/state/workers/child-implementer.sock",
      },
    };
    const client = fakeWorkerRpcClient();
    // A real client's own `getState()` call (in `spawnWorker`'s alive check, right before this)
    // would have already seeded `runState` from `isStreaming` - idle, since this worker has
    // nothing in flight to interrupt. Only an idle client is prompted directly (see
    // `WorkerAdmission.resumeOrQueueExisting`); a fake that never reports state stays at its
    // default "unknown" otherwise, which is no longer sufficient on its own.
    client.setRunStateSilently("idle");
    const original = exception(token).original;
    const { manager: processes, publications } = manager(state, {
      connectWorkerRpc: async () => client,
    });

    await processes.handleException(exception(token, original));

    expect(client.prompts).toEqual([JSON.stringify({ type: "catchup-worker", unhandled: [] })]);
    expect(publications).toEqual([]);
  });

  it("resumes a dead worker with --resume through spawnWorker and never spawns a role with no locator", async () => {
    const state = newLegionState("omp", 1);
    tree(state);
    state.issues[child] = {
      key: child,
      title: "Child",
      status: "in_progress",
      parent: root,
      children: [],
    };
    const role: LegionRole = "implementer";
    const token = roleToken("omp", child, role);
    const stateDir = await temporaryDir();
    const sessionFile = path.join(stateDir, "child-implementer.jsonl");
    await writeFile(sessionFile, "{}", "utf8");
    state.roles[token] = {
      issue: child,
      role,
      sessionId: "ses_implementer",
      generation: 1,
      locator: {
        tmuxSession: "legion-omp",
        tmuxWindowId: "@42",
        tmuxPaneId: "%2",
        socketPath: "/state/workers/child-implementer.sock",
        ompSessionFile: sessionFile,
      },
    };
    const original = exception(token).original;
    const {
      manager: processes,
      publications,
      commands,
    } = manager(state, {
      config: config(stateDir),
      connectWorkerRpc: async () => {
        throw new Error("dead shim socket");
      },
    });

    await processes.handleException(exception(token, original));

    // A respawned worker's catch-up is queued as its pendingAssignment (delivered over its
    // socket once it confirms boot via /worker/ready) rather than published over NATS: nothing
    // is published here, and never a replay of the raw missed event.
    expect(publications).toEqual([]);
    expect(commands.some((command) => command.join(" ").includes("--resume"))).toBe(true);
    expect(state.roles[token]).toMatchObject({
      generation: 2,
      pendingAssignment: JSON.stringify({ type: "catchup-worker", unhandled: [] }),
    });

    // An exception on a role no claim has ever backed (never spawned) is a no-op: nothing to
    // resume, and never a fresh spawn from a missed-wake signal alone.
    const unbackedToken = roleToken("omp", child, "reviewer");
    publications.length = 0;
    commands.length = 0;
    await processes.handleException(exception(unbackedToken, exception(unbackedToken).original));
    expect(publications).toEqual([]);
    expect(commands).toEqual([]);
  });

  it("relaunches with --resume when a claim's locator was already cleared by markWorkerDeadLocked but its resumeSessionFile survives — the exact shape a confirmed-dead worker leaves behind for the next no-holder recovery", async () => {
    const state = newLegionState("omp", 1);
    tree(state);
    state.issues[child] = {
      key: child,
      title: "Child",
      parent: root,
      children: [],
      status: "in_progress",
    };
    const role: LegionRole = "implementer";
    const token = roleToken("omp", child, role);
    const stateDir = await temporaryDir();
    const sessionFile = path.join(stateDir, "child-implementer.jsonl");
    await writeFile(sessionFile, "{}", "utf8");
    // Exactly `markWorkerDeadLocked`'s own output shape (processes.ts:662-671): locator
    // deleted, resumeSessionFile carried forward from the dead locator's own ompSessionFile.
    // No prior fix (before this round) resumed this claim at all — `resumeWorker`'s own guard
    // required a locator, so a no-holder exception delivered after the worker was already
    // confirmed dead silently no-op'd forever, stranding the role.
    state.roles[token] = {
      issue: child,
      role,
      generation: 2,
      resumeSessionFile: sessionFile,
    };
    const original = exception(token).original;
    const {
      manager: processes,
      publications,
      commands,
    } = manager(state, {
      config: config(stateDir),
      connectWorkerRpc: async () => {
        throw new Error("dead shim socket");
      },
    });

    await processes.handleException(exception(token, original));

    expect(publications).toEqual([]);
    expect(commands.some((command) => command.join(" ").includes("--resume"))).toBe(true);
    expect(state.roles[token]).toMatchObject({
      generation: 3,
      pendingAssignment: JSON.stringify({ type: "catchup-worker", unhandled: [] }),
    });
    const relaunched = state.roles[token];
    if (!relaunched || !("issue" in relaunched) || !relaunched.locator) {
      throw new Error("expected a fresh locator after relaunch");
    }
    expect(relaunched.locator.ompSessionFile).toBe(sessionFile);
  });

  it("publishes worker-died to the tree architect once resume attempts exhaust the launch-failure threshold", async () => {
    const state = newLegionState("omp", 1);
    tree(state);
    state.issues[child] = {
      key: child,
      title: "Child",
      status: "in_progress",
      parent: root,
      children: [],
    };
    const role: LegionRole = "implementer";
    const token = roleToken("omp", child, role);
    const stateDir = await temporaryDir();
    const sessionFile = path.join(stateDir, "child-implementer.jsonl");
    await writeFile(sessionFile, "{}", "utf8");
    state.roles[token] = {
      issue: child,
      role,
      sessionId: "ses_implementer",
      generation: 1,
      launchFailures: 2,
      locator: {
        tmuxSession: "legion-omp",
        tmuxWindowId: "@42",
        tmuxPaneId: "%2",
        socketPath: "/state/workers/child-implementer.sock",
        ompSessionFile: sessionFile,
      },
    };
    const original = exception(token).original;
    const { manager: processes, publications } = manager(state, {
      config: config(stateDir),
      // The dead pane's PID-liveness poll never sees it die (readProcessCmdline reports an
      // always-alive OMP pane), so it always exhausts every retirement poll attempt; without
      // this override that is 20 real 100ms waits, flaky under load against bun's 5s per-test
      // timeout for a cost this test has no reason to pay.
      sleep: async () => {},
      connectWorkerRpc: async () => {
        throw new Error("dead shim socket");
      },
      run: async (command) => {
        if (command[0] === "jj" && command[1] === "workspace" && command[2] === "add") {
          throw new Error("workspace provisioning is broken");
        }
        return liveRun(command);
      },
    });

    await processes.handleException(exception(token, original));

    expect(publications).toContainEqual({
      subject: roleTopic(roleToken("omp", root, "architect")),
      json: JSON.stringify({ type: "worker-died", issue: child, role }),
    });
  });

  it("cancelBootWatchdog stops an armed watchdog from retiring and retrying a still-booting worker", async () => {
    const state = newLegionState("omp", 1);
    tree(state);
    state.issues[child] = {
      key: child,
      title: "Child",
      status: "in_progress",
      parent: root,
      children: [],
    };
    const role: LegionRole = "implementer";
    const token = roleToken("omp", child, role);
    const stateDir = await temporaryDir();
    let currentTime = Date.parse("2026-08-24T00:00:00.000Z");
    const {
      manager: processes,
      publications,
      state: managedState,
    } = manager(state, {
      config: config(stateDir, { workerBootTimeoutSeconds: 1 }),
      now: () => currentTime,
      sleep: async (ms) => {
        currentTime += ms;
        await onceEventLoop();
      },
      connectWorkerRpc: async () => {
        throw new Error("shim not listening yet");
      },
    });

    const response = await processes.spawnWorker(root, child, role, "do the work");
    expect(response.status).toBe("spawned");
    const claim = managedState.roles[token];
    if (!claim || !("issue" in claim)) throw new Error("claim missing after spawn");
    processes.cancelBootWatchdog(token, claim.generation);

    // Drains the just-cancelled watchdog's in-flight connect-retry loop deterministically
    // (yielding the event loop, never guessing a real-time wait), rather than racing an
    // unsettled background promise.
    await flushEventLoop();

    expect(publications).toEqual([]);
    const finalClaim = managedState.roles[token];
    expect(finalClaim && "issue" in finalClaim ? finalClaim.launchFailures : undefined).toBe(0);
  });
  it("retires a started worker whose ready delivery cannot connect, then delivers its original assignment after the resumed retry confirms ready", async () => {
    const state = newLegionState("omp", 1);
    tree(state);
    state.issues[child] = {
      key: child,
      title: "Child",
      status: "in_progress",
      parent: root,
      children: [],
    };
    const role: LegionRole = "implementer";
    const token = roleToken("omp", child, role);
    const stateDir = await temporaryDir();
    const priorSession = path.join(stateDir, "prior-implementer-session.json");
    await writeFile(priorSession, "{}", "utf8");
    let currentTime = Date.parse("2026-08-24T00:00:00.000Z");
    const readyClient = fakeWorkerRpcClient();
    const {
      manager: processes,
      commands,
      state: managedState,
    } = manager(state, {
      config: config(stateDir, {
        workerBootTimeoutSeconds: 1,
        // The first generation is dead, so it retires on its first deadline. The retry's socket
        // accepts connections while its /worker/started -> /worker/ready handshake runs, so it
        // must not be treated as a perpetually unregistered boot during this test.
        workerBootRegistrationDeadlineIntervals: 1_000,
      }),
      now: () => currentTime,
      sleep: async (ms) => {
        currentTime += ms;
        await onceEventLoop();
      },
      connectWorkerRpc: async () => {
        const claim = managedState.roles[token];
        if (!claim || !("issue" in claim) || claim.generation === 1) {
          throw new Error("first-generation shim cannot connect");
        }
        return readyClient;
      },
      readProcessCmdline: async () => "bash\0",
    });

    await processes.spawnWorker(root, child, role, "implement #43");
    const started = managedState.roles[token];
    if (!started || !("issue" in started) || !started.locator) {
      throw new Error("first-generation claim missing");
    }
    // Models /worker/started: the session capability must exist before /worker/ready can be
    // attempted, but this deliberately does not mark the ready delivery as confirmed.
    started.sessionId = "ses_implementer";
    started.locator.ompSessionFile = priorSession;

    await expect(processes.workerReady(child, role, "ses_implementer", 1)).rejects.toThrow(
      "first-generation shim cannot connect"
    );

    await flushEventLoopUntil(() => {
      const claim = managedState.roles[token];
      return (
        claim !== undefined &&
        "issue" in claim &&
        claim.generation === 2 &&
        claim.launchFailures === 1 &&
        claim.locator !== undefined
      );
    }, 50_000);

    const relaunched = managedState.roles[token];
    if (!relaunched || !("issue" in relaunched) || !relaunched.locator) {
      throw new Error("relaunched claim missing");
    }
    expect(relaunched.pendingAssignment).toBe("implement #43");
    expect(relaunched.launchFailures).toBe(1);
    expect(commands.some((command) => command.join(" ").includes("--resume"))).toBeTrue();

    // Models the relaunched worker's /worker/started transition: the session capability is
    // minted, but launch-failure accounting is deliberately left untouched here -- only a
    // durably confirmed /worker/ready (the following workerReady call) resets it.
    relaunched.sessionId = "ses_implementer";
    await processes.workerReady(child, role, "ses_implementer", relaunched.generation ?? 0);

    expect(readyClient.prompts).toEqual(["implement #43"]);
    expect(relaunched.pendingAssignment).toBeUndefined();
    expect(relaunched.launchFailures).toBeUndefined();
  });

  it("retires and retries a worker whose boot the watchdog's timeout never sees /worker/started confirm, escalating to worker-died at the launch-failure threshold", async () => {
    const state = newLegionState("omp", 1);
    tree(state);
    state.issues[child] = {
      key: child,
      title: "Child",
      status: "in_progress",
      parent: root,
      children: [],
    };
    const role: LegionRole = "implementer";
    const token = roleToken("omp", child, role);
    const stateDir = await temporaryDir();
    let currentTime = Date.parse("2026-08-24T00:00:00.000Z");
    const {
      manager: processes,
      publications,
      commands,
      state: managedState,
    } = manager(state, {
      config: config(stateDir, { workerBootTimeoutSeconds: 1 }),
      now: () => currentTime,
      sleep: async (ms) => {
        currentTime += ms;
        await onceEventLoop();
      },
      connectWorkerRpc: async () => {
        throw new Error("shim never listens");
      },
      // The boot watchdog now probes pane liveness before evicting a slow boot (see
      // `armBootWatchdog`/`probeWorkerAlive`): a dead shim alone is no longer sufficient to be
      // "dead" unless the pane is gone too, so this simulates a non-OMP (dead/replaced)
      // process at the recorded pid, matching what the socket refusal already implies.
      readProcessCmdline: async () => "bash\0",
    });

    await processes.spawnWorker(root, child, role, "do the work");

    // The watchdog's connect-retry and retire-polling loops drive entirely off the injected
    // fake clock/sleep, never a real timer, so three full workerBootTimeoutSeconds cycles (each
    // retrying with a fresh pane the fixture's default command runner always opens successfully)
    // drain deterministically without any real wait, despite simulating minutes of elapsed boot
    // time. Each below-threshold retry now goes through the normal admission queue (see
    // `WorkerAdmission.enqueueForRetry`), so it publishes its own `worker-started` exactly like
    // any other successful relaunch — flushing on the *first* publication would stop after that
    // intermediate one, so this waits specifically for the terminal `worker-died`.
    await flushEventLoopUntil(
      () =>
        publications.some(
          (publication) =>
            publication.subject === roleTopic(roleToken("omp", root, "architect")) &&
            publication.json.includes("worker-died")
        ),
      50_000
    );

    expect(commands.some((command) => command[0] === "kill")).toBe(true);
    expect(publications).toContainEqual({
      subject: roleTopic(roleToken("omp", root, "architect")),
      json: JSON.stringify({ type: "worker-died", issue: child, role }),
    });
    const finalClaim = managedState.roles[token];
    expect(finalClaim && "issue" in finalClaim ? finalClaim.launchFailures : undefined).toBe(3);
  });

  it("never evicts a slow-but-alive boot across repeated observation intervals; a later confirmation stops the watch", async () => {
    const state = newLegionState("omp", 1);
    tree(state);
    state.issues[child] = {
      key: child,
      title: "Child",
      status: "in_progress",
      parent: root,
      children: [],
    };
    const role: LegionRole = "implementer";
    const token = roleToken("omp", child, role);
    const stateDir = await temporaryDir();
    let currentTime = Date.parse("2026-08-24T00:00:00.000Z");
    let connectAttempts = 0;
    const {
      manager: processes,
      publications,
      state: managedState,
    } = manager(state, {
      config: config(stateDir, {
        workerBootTimeoutSeconds: 1,
        // High enough that 3 full observation cycles (this test's own scenario) never hits the
        // registration deadline -- this test is about the alive-vs-dead probe, not the deadline.
        workerBootRegistrationDeadlineIntervals: 1_000,
      }),
      now: () => currentTime,
      sleep: async (ms) => {
        currentTime += ms;
        await onceEventLoop();
      },
      // The shim never answers (a real, slow OMP startup that has not yet opened its RPC
      // socket) — but the fixture's default `run`/`readProcessCmdline` report a genuinely
      // alive OMP pane throughout, so the watchdog's probe must never treat this as dead.
      connectWorkerRpc: async () => {
        connectAttempts += 1;
        throw new Error("shim not listening yet");
      },
    });

    await processes.spawnWorker(root, child, role, "do the work");

    const startTime = currentTime;
    // Three full workerBootTimeoutSeconds cycles' worth of ticks (each interval's own
    // connect-retry loop alone needs ~10 fake-clock-driven sleeps at this timeout), driven
    // entirely off the injected fake clock/sleep plus the watchdog's own real macrotask yield
    // on every re-arm (see `armBootWatchdog`'s doc comment) - no real wait despite simulating
    // several seconds of elapsed boot time. Stops as soon as the fake clock has advanced far
    // enough rather than guessing a fixed tick count for a chain whose exact length (connect
    // retries plus a real macrotask yield per re-arm) isn't known.
    await flushEventLoopUntil(() => currentTime - startTime >= 3_000, 20_000);

    expect(publications).toEqual([]);
    const stillBooting = managedState.roles[token];
    if (!stillBooting || !("issue" in stillBooting)) throw new Error("claim missing");
    // Never evicted, however many intervals passed: no retirement, no launch-failure count.
    expect(stillBooting.locator).toBeDefined();
    expect(stillBooting.launchFailures).toBe(0);
    expect(currentTime - startTime).toBeGreaterThanOrEqual(3_000);

    // Ready confirmation cancels the watch outright rather than waiting for its next wake.
    stillBooting.sessionId = "ses_implementer";
    stillBooting.readyConfirmedAt = currentTime;
    processes.cancelBootWatchdog(token, stillBooting.generation);
    const attemptsAtConfirmation = connectAttempts;
    await flushEventLoop(400);

    expect(connectAttempts).toBe(attemptsAtConfirmation);
    expect(publications).toEqual([]);
  });

  it("re-arms (never retires) a boot whose pane is gone but whose shim socket still connects and negotiates, even when its get_state call rejects", async () => {
    const state = newLegionState("omp", 1);
    tree(state);
    state.issues[child] = {
      key: child,
      title: "Child",
      status: "in_progress",
      parent: root,
      children: [],
    };
    const role: LegionRole = "implementer";
    const token = roleToken("omp", child, role);
    const stateDir = await temporaryDir();
    let currentTime = Date.parse("2026-08-24T00:00:00.000Z");
    let getStateCalls = 0;
    const client = {
      ...fakeWorkerRpcClient(),
      getState: async () => {
        getStateCalls += 1;
        throw new Error("get_state timed out");
      },
    };
    const {
      manager: processes,
      publications,
      state: managedState,
    } = manager(state, {
      config: config(stateDir, {
        workerBootTimeoutSeconds: 1,
        // High enough that 3 full observation cycles (this test's own scenario) never hits the
        // registration deadline -- this test is about the alive-vs-dead probe, not the deadline.
        workerBootRegistrationDeadlineIntervals: 1_000,
      }),
      now: () => currentTime,
      sleep: async (ms) => {
        currentTime += ms;
        await onceEventLoop();
      },
      // The recorded pane is gone throughout (a real dead-pane liveness check), but the shim
      // socket still accepts a connection and negotiates — the watchdog must treat this exactly
      // like a merely-busy shim, never as a dead boot, however many times `get_state` itself
      // then rejects.
      readProcessCmdline: async () => "bash\0",
      run: async (command) => {
        if (command[0] === "tmux" && command[1] === "new-window") {
          return { stdout: "@42 %1 12345\n", exitCode: 0 };
        }
        if (command[0] === "tmux" && command[1] === "list-panes") {
          return { stdout: "", exitCode: 1 };
        }
        return { stdout: "", exitCode: 0 };
      },
      connectWorkerRpc: async () => client,
    });

    await processes.spawnWorker(root, child, role, "do the work");

    const startTime = currentTime;
    await flushEventLoopUntil(() => currentTime - startTime >= 3_000, 20_000);

    expect(publications).toEqual([]);
    expect(getStateCalls).toBeGreaterThan(0);
    const stillBooting = managedState.roles[token];
    if (!stillBooting || !("issue" in stillBooting)) throw new Error("claim missing");
    // Never retired, however many intervals passed and however many `get_state` calls
    // rejected: no locator clear, no launch-failure count.
    expect(stillBooting.locator).toBeDefined();
    expect(stillBooting.launchFailures).toBe(0);
    expect(currentTime - startTime).toBeGreaterThanOrEqual(3_000);
  });

  it("dispose() stops every armed boot watchdog's connect-retry loop for good, not merely until its next check", async () => {
    const state = newLegionState("omp", 1);
    tree(state);
    state.issues[child] = {
      key: child,
      title: "Child",
      status: "in_progress",
      parent: root,
      children: [],
    };
    const implementerToken = roleToken("omp", child, "implementer");
    const testerToken = roleToken("omp", child, "tester");
    const stateDir = await temporaryDir();
    let connectAttempts = 0;
    const {
      manager: processes,
      publications,
      state: managedState,
    } = manager(state, {
      // The production default (120s): with `now` left at the fixture's fixed clock, the
      // watchdog's connect-retry loop can never reach its own deadline on its own — only
      // `dispose()`'s cancellation stops it, so a continuing rise in `connectAttempts` after
      // dispose() unambiguously means a live loop survived it.
      config: config(stateDir),
      connectWorkerRpc: async () => {
        connectAttempts += 1;
        throw new Error("shim not listening yet");
      },
      sleep: async () => {
        await onceEventLoop();
      },
    });

    await processes.spawnWorker(root, child, "implementer", "implement it");
    await processes.spawnWorker(root, child, "tester", "test it");
    // Let both watchdogs' connect-retry loops genuinely spin for a while first.
    await flushEventLoop(200);
    const attemptsBeforeDispose = connectAttempts;
    expect(attemptsBeforeDispose).toBeGreaterThan(0);

    processes.dispose();
    const attemptsAtDispose = connectAttempts;

    await flushEventLoop();

    expect(connectAttempts).toBe(attemptsAtDispose);
    expect(publications).toEqual([]);
    const implementerClaim = managedState.roles[implementerToken];
    const testerClaim = managedState.roles[testerToken];
    expect(
      implementerClaim && "issue" in implementerClaim ? implementerClaim.launchFailures : undefined
    ).toBe(0);
    expect(testerClaim && "issue" in testerClaim ? testerClaim.launchFailures : undefined).toBe(0);
  });

  it("reports a reclaim-architect nack to the controller without a worker-side revival path", async () => {
    const state = newLegionState("omp", 1);
    tree(state);
    const { manager: processes, publications } = manager(state, {
      run: liveRun,
      natsRequest: async () =>
        JSON.stringify({ type: "nack", error: "architect transcript is missing" }),
    });

    await processes.handleException(exception(roleToken("omp", root, "architect")));

    expect(publications).toEqual([
      {
        subject: `notifications.role.${controllerToken("omp")}`,
        json: JSON.stringify({
          type: "revive-failed",
          issue: root,
          role: "architect",
        }),
      },
    ]);
  });

  it("spawns one controller window when the controller delivery fails", async () => {
    const stateDir = await temporaryDir();
    const state = newLegionState("omp", 1);
    let controllerSpawned = false;
    const {
      manager: processes,
      commands,
      publications,
    } = manager(state, {
      config: config(stateDir),
      run: async (command) => {
        commands.push(command);
        if (command[1] === "list-windows") {
          return {
            stdout: controllerSpawned ? "controller\n" : "",
            exitCode: 0,
          };
        }
        if (command[1] === "list-panes") {
          return controllerSpawned
            ? { stdout: "12345\n", exitCode: 0 }
            : { stdout: "", exitCode: 1 };
        }
        if (command[0] === "kill") return { stdout: "", exitCode: 0 };
        if (command[1] === "new-window") {
          controllerSpawned = true;
          return { stdout: "@42 %1 12345\n", exitCode: 0 };
        }
        if (command[1] === "has-session") return { stdout: "", exitCode: 0 };
        return { stdout: "", exitCode: 0 };
      },
    });

    await processes.handleException(
      exception(controllerToken("omp"), {
        topic: "notifications.github.sjawhar.legion.issue.42.comment",
        payload: "{}",
        eventId: "evt-controller",
      })
    );

    expect(commands.filter((command) => command[1] === "new-window")).toHaveLength(1);
    expect(state.controllerLocator).toEqual({
      tmuxSession: "legion-omp",
      tmuxWindowId: "@42",
      tmuxPaneId: "%1",
      socketPath: path.join(stateDir, "workers", "controller.sock"),
    });
    expect(publications).toEqual([]);
  });

  it("mints a fresh controller capability only for each controller window spawn", async () => {
    const stateDir = await temporaryDir();
    const state = newLegionState("omp", 1);
    const commands: string[][] = [];
    let controllerLive = false;
    let mints = 0;
    const { manager: processes } = manager(state, {
      config: config(stateDir),
      mintControllerCapability: async () => `controller-secret-${++mints}`,
      run: async (command) => {
        commands.push(command);
        if (command[1] === "list-windows") {
          return { stdout: controllerLive ? "controller\n" : "", exitCode: 0 };
        }
        if (command[1] === "list-panes") {
          return controllerLive ? { stdout: "12345\n", exitCode: 0 } : { stdout: "", exitCode: 1 };
        }
        if (command[0] === "kill") return { stdout: "", exitCode: 0 };
        if (command[1] === "new-window") {
          controllerLive = true;
          return { stdout: "@42 %1 12345\n", exitCode: 0 };
        }
        return { stdout: "", exitCode: 0 };
      },
    });

    await processes.ensureController();
    controllerLive = false;
    await processes.ensureController();

    const windows = commands.filter((command) => command[1] === "new-window");
    expect(mints).toBe(2);
    expect(windows[0]).toContain("LEGION_CONTROLLER_SECRET=controller-secret-1");
    expect(windows[1]).toContain("LEGION_CONTROLLER_SECRET=controller-secret-2");
  });

  it("ensureController's registration-deadline callback leaves an alive controller alone once its role claim arrives before the deadline elapses", async () => {
    const stateDir = await temporaryDir();
    const state = newLegionState("omp", 1);
    state.controllerLocator = {
      tmuxSession: "legion-omp",
      tmuxWindowId: "@controller",
      tmuxPaneId: "%1",
      socketPath: path.join(stateDir, "workers", "controller.sock"),
    };
    const sleepGate = Promise.withResolvers<void>();
    const commands: string[][] = [];
    const { manager: processes, state: managedState } = manager(state, {
      config: config(stateDir),
      sleep: async () => {
        await sleepGate.promise;
      },
      connectWorkerRpc: async () => {
        throw new Error("ECONNREFUSED");
      },
      run: async (command) => {
        commands.push(command);
        if (command[1] === "list-panes") return { stdout: "12345\n", exitCode: 0 };
        if (command[1] === "new-window") return { stdout: "@43 %2 54321\n", exitCode: 0 };
        return { stdout: "", exitCode: 0 };
      },
    });

    // Alive pane, no role claim yet: arms the registration deadline instead of treating this as
    // "there's nothing to do".
    await processes.ensureController();
    // The role claim arrives (what a real `/controller/ready` does) before the deadline elapses.
    managedState.roles[controllerToken("omp")] = {
      role: "controller",
      sessionId: "ses-controller",
    };
    // Only now does the deadline elapse -- its own callback must re-check the role rather than
    // trust whatever was true when it was armed.
    sleepGate.resolve();
    await flushEventLoop();

    expect(commands.some((command) => command[1] === "new-window")).toBe(false);
    expect(commands.some((command) => command[1] === "kill-pane")).toBe(false);
    expect(managedState.controllerLocator).toEqual(state.controllerLocator);
  });

  it("ensureController retires a stuck controller pane and spawns a fresh one once its registration deadline elapses with no role claim", async () => {
    const stateDir = await temporaryDir();
    const state = newLegionState("omp", 1);
    const staleLocator = {
      tmuxSession: "legion-omp",
      tmuxWindowId: "@controller",
      tmuxPaneId: "%1",
      socketPath: path.join(stateDir, "workers", "controller.sock"),
    };
    state.controllerLocator = { ...staleLocator };
    const sleepGate = Promise.withResolvers<void>();
    let sleepCalls = 0;
    const commands: string[][] = [];
    const { manager: processes, state: managedState } = manager(state, {
      config: config(stateDir),
      // Only the first armed deadline (for the stale locator this test is about) is under this
      // test's control; the fresh controller `retireAndRespawnStuckController` spawns also arms
      // its own deadline (see `ensureController`'s doc comment), which must stay pending here --
      // otherwise, since `sleepGate` is already resolved by then, it would elapse immediately
      // too and retire the fresh pane this test is asserting survived.
      sleep: async () => {
        sleepCalls += 1;
        if (sleepCalls === 1) {
          await sleepGate.promise;
          return;
        }
        await new Promise<void>(() => {});
      },
      connectWorkerRpc: async () => {
        throw new Error("ECONNREFUSED");
      },
      run: async (command) => {
        commands.push(command);
        if (command[1] === "list-panes") return { stdout: "12345\n", exitCode: 0 };
        if (command[1] === "new-window") return { stdout: "@44 %3 65432\n", exitCode: 0 };
        if (command[0] === "tmux" && command[1] === "kill-pane") return { stdout: "", exitCode: 0 };
        return { stdout: "", exitCode: 0 };
      },
    });

    // Alive pane, no role claim: arms the registration deadline.
    await processes.ensureController();
    // The role never gets claimed -- the deadline elapses with nothing having changed.
    sleepGate.resolve();
    // The respawn chain routes through real fs I/O (spawnController's own config write), which
    // can take more than a fixed microtask/macrotask budget under load -- waits specifically for
    // its terminal effect (the fresh window opening) rather than guessing a tick count.
    await flushEventLoopUntil(
      () =>
        managedState.controllerLocator !== undefined &&
        managedState.controllerLocator.tmuxWindowId !== staleLocator.tmuxWindowId,
      20_000
    );

    // The stuck pane was retired (no graceful shim response, so straight to kill-pane) and a
    // fresh one spawned in its place.
    const killPaneRan = commands.some(
      (command) => command[0] === "tmux" && command[1] === "kill-pane"
    );
    expect(killPaneRan).toBe(true);
    expect(commands.some((command) => command[1] === "new-window")).toBe(true);
    expect(managedState.controllerLocator).toEqual({
      tmuxSession: "legion-omp",
      tmuxWindowId: "@44",
      tmuxPaneId: "%3",
      socketPath: path.join(stateDir, "workers", "controller.sock"),
    });
  });

  it("dispose() cancels a pending controller-registration deadline so its stale expiry never retires or respawns", async () => {
    const stateDir = await temporaryDir();
    const state = newLegionState("omp", 1);
    const locator = {
      tmuxSession: "legion-omp",
      tmuxWindowId: "@controller",
      tmuxPaneId: "%1",
      socketPath: path.join(stateDir, "workers", "controller.sock"),
    };
    state.controllerLocator = { ...locator };
    const sleepGate = Promise.withResolvers<void>();
    const commands: string[][] = [];
    const { manager: processes, state: managedState } = manager(state, {
      config: config(stateDir),
      sleep: async () => {
        await sleepGate.promise;
      },
      connectWorkerRpc: async () => {
        throw new Error("ECONNREFUSED");
      },
      run: async (command) => {
        commands.push(command);
        if (command[1] === "list-panes") return { stdout: "12345\n", exitCode: 0 };
        if (command[1] === "new-window") return { stdout: "@45 %4 76543\n", exitCode: 0 };
        if (command[0] === "tmux" && command[1] === "kill-pane") return { stdout: "", exitCode: 0 };
        return { stdout: "", exitCode: 0 };
      },
    });

    await processes.ensureController(); // arms the deadline
    processes.dispose();
    // The underlying wait may still be pending (a test's injected sleep has no real timer to
    // cancel), but dispose() must have cleared the tracking `cancelControllerRegistrationDeadline`
    // relies on, so this stale fire is recognized as such and does nothing.
    sleepGate.resolve();
    await flushEventLoop();

    expect(commands.some((command) => command[1] === "kill-pane")).toBe(false);
    expect(commands.some((command) => command[1] === "new-window")).toBe(false);
    expect(managedState.controllerLocator).toEqual(locator);
  });

  it("arms a fresh registration deadline for a controller spawned from scratch, so one that itself never registers also gets retried", async () => {
    const stateDir = await temporaryDir();
    const state = newLegionState("omp", 1);
    let sleepCalls = 0;
    const firstSleepGate = Promise.withResolvers<void>();
    const commands: string[][] = [];
    let windowCount = 0;
    const { manager: processes, state: managedState } = manager(state, {
      config: config(stateDir),
      // Only the very first armed deadline (for the from-scratch spawn this test is about) is
      // under this test's control; the *second* fresh spawn (once the first is retired) arms its
      // own deadline too, which must stay pending so this test's own assertions see a stable
      // result after exactly one retry cycle.
      sleep: async () => {
        sleepCalls += 1;
        if (sleepCalls === 1) {
          await firstSleepGate.promise;
          return;
        }
        await new Promise<void>(() => {});
      },
      connectWorkerRpc: async () => {
        throw new Error("ECONNREFUSED");
      },
      run: async (command) => {
        commands.push(command);
        if (command[1] === "list-panes") return { stdout: "12345\n", exitCode: 0 };
        if (command[1] === "new-window") {
          windowCount += 1;
          return { stdout: `@5${windowCount} %${windowCount} 8765${windowCount}\n`, exitCode: 0 };
        }
        if (command[0] === "tmux" && command[1] === "kill-pane") return { stdout: "", exitCode: 0 };
        return { stdout: "", exitCode: 0 };
      },
    });

    // No locator at all: spawns fresh outright, then must arm a deadline for THIS spawn too --
    // not merely for a controller that was already alive when observed.
    await processes.ensureController();
    const firstLocator = managedState.controllerLocator;
    expect(firstLocator).toBeDefined();
    expect(windowCount).toBe(1);

    // The freshly-spawned controller never registers either -- its own armed deadline elapses.
    firstSleepGate.resolve();
    await flushEventLoopUntil(() => windowCount >= 2, 20_000);

    expect(commands.some((command) => command[0] === "tmux" && command[1] === "kill-pane")).toBe(
      true
    );
    expect(windowCount).toBe(2);
    expect(managedState.controllerLocator?.tmuxWindowId).not.toBe(firstLocator?.tmuxWindowId);
  });

  it("a stale registration-deadline expiry no-ops once a newer locator has replaced the one it observed", async () => {
    const stateDir = await temporaryDir();
    const state = newLegionState("omp", 1);
    const staleLocator = {
      tmuxSession: "legion-omp",
      tmuxWindowId: "@controller",
      tmuxPaneId: "%1",
      socketPath: path.join(stateDir, "workers", "controller.sock"),
    };
    state.controllerLocator = { ...staleLocator };
    const staleSleepGate = Promise.withResolvers<void>();
    let sleepCalls = 0;
    let panesAlive = true;
    const commands: string[][] = [];
    let windowCount = 0;
    const { manager: processes, state: managedState } = manager(state, {
      config: config(stateDir),
      // Only the *first* armed wait (for `staleLocator`) is under this test's control; the fresh
      // replacement spawned below arms its own separate wait, which must stay pending here -- a
      // shared gate would resolve both simultaneously and make this test's own respawn
      // indistinguishable from the stale callback wrongly acting a second time.
      sleep: async () => {
        sleepCalls += 1;
        if (sleepCalls === 1) {
          await staleSleepGate.promise;
          return;
        }
        await new Promise<void>(() => {});
      },
      connectWorkerRpc: async () => {
        throw new Error("ECONNREFUSED");
      },
      run: async (command) => {
        commands.push(command);
        if (command[1] === "list-panes") {
          return panesAlive ? { stdout: "12345\n", exitCode: 0 } : { stdout: "", exitCode: 1 };
        }
        if (command[1] === "new-window") {
          windowCount += 1;
          return { stdout: `@6${windowCount} %${windowCount} 9876${windowCount}\n`, exitCode: 0 };
        }
        if (command[0] === "tmux" && command[1] === "kill-pane") return { stdout: "", exitCode: 0 };
        return { stdout: "", exitCode: 0 };
      },
    });

    // Arms the stale wait for `staleLocator`.
    await processes.ensureController();
    // The stale pane dies on its own, independent of the registration deadline: the next
    // `ensureController` call observes this directly, cancels the stale wait, and spawns a
    // fresh replacement (arming its own, separately-tracked wait for it).
    panesAlive = false;
    await processes.ensureController();
    const freshLocator = managedState.controllerLocator;
    expect(freshLocator).toBeDefined();
    expect(freshLocator?.tmuxWindowId).not.toBe(staleLocator.tmuxWindowId);
    panesAlive = true;

    // The stale wait's own timer finally fires, late -- it must recognize itself as superseded
    // and touch neither the fresh locator nor spawn yet another replacement.
    staleSleepGate.resolve();
    await flushEventLoop();

    expect(windowCount).toBe(1);
    expect(
      commands.filter((command) => command[0] === "tmux" && command[1] === "kill-pane")
    ).toHaveLength(0);
    expect(managedState.controllerLocator).toEqual(freshLocator);
  });

  it("a role claim landing during the post-deadline liveness re-check wins over the stale-timeout decision, leaving the pane untouched", async () => {
    const stateDir = await temporaryDir();
    const state = newLegionState("omp", 1);
    const locator = {
      tmuxSession: "legion-omp",
      tmuxWindowId: "@controller",
      tmuxPaneId: "%1",
      socketPath: path.join(stateDir, "workers", "controller.sock"),
    };
    state.controllerLocator = { ...locator };
    const sleepGate = Promise.withResolvers<void>();
    const commands: string[][] = [];
    let listPanesCalls = 0;
    const { manager: processes, state: managedState } = manager(state, {
      config: config(stateDir),
      sleep: async () => {
        await sleepGate.promise;
      },
      connectWorkerRpc: async () => {
        throw new Error("ECONNREFUSED");
      },
      run: async (command) => {
        commands.push(command);
        if (command[1] === "list-panes") {
          listPanesCalls += 1;
          // The 1st call is `ensureController`'s own initial liveness check (before the deadline
          // is even armed); the 2nd is `retireAndRespawnStuckController`'s post-deadline
          // re-check. A real `/controller/ready` lands exactly during that 2nd call's own await.
          if (listPanesCalls === 2) {
            managedState.roles[controllerToken("omp")] = {
              role: "controller",
              sessionId: "ses-controller",
            };
          }
          return { stdout: "12345\n", exitCode: 0 };
        }
        if (command[1] === "new-window") return { stdout: "@70 %9 111111\n", exitCode: 0 };
        if (command[0] === "tmux" && command[1] === "kill-pane") return { stdout: "", exitCode: 0 };
        return { stdout: "", exitCode: 0 };
      },
    });

    await processes.ensureController();
    sleepGate.resolve();
    await flushEventLoop();

    expect(listPanesCalls).toBeGreaterThanOrEqual(2);
    expect(commands.some((command) => command[1] === "kill-pane")).toBe(false);
    expect(commands.some((command) => command[1] === "new-window")).toBe(false);
    expect(managedState.controllerLocator).toEqual(locator);
  });

  it("leaves the stale locator in place when the stop/kill attempt fails, instead of orphaning a still-live pane with no controller ever spawned onto it", async () => {
    const stateDir = await temporaryDir();
    const state = newLegionState("omp", 1);
    const locator = {
      tmuxSession: "legion-omp",
      tmuxWindowId: "@controller",
      tmuxPaneId: "%1",
      socketPath: path.join(stateDir, "workers", "controller.sock"),
    };
    state.controllerLocator = { ...locator };
    const sleepGate = Promise.withResolvers<void>();
    const commands: string[][] = [];
    const { manager: processes, state: managedState } = manager(state, {
      config: config(stateDir),
      sleep: async () => {
        await sleepGate.promise;
      },
      connectWorkerRpc: async () => {
        throw new Error("ECONNREFUSED");
      },
      run: async (command) => {
        commands.push(command);
        if (command[1] === "list-panes") return { stdout: "12345\n", exitCode: 0 };
        if (command[0] === "tmux" && command[1] === "kill-pane") {
          return { stdout: "", stderr: "tmux: unable to kill pane", exitCode: 1 };
        }
        if (command[1] === "new-window") return { stdout: "@71 %10 222222\n", exitCode: 0 };
        return { stdout: "", exitCode: 0 };
      },
    });

    await processes.ensureController();
    sleepGate.resolve();
    await flushEventLoop();

    // The kill-pane attempt ran and failed, but the locator survives exactly as it was -- never
    // cleared, and no second controller spawned onto what may still be a live pane.
    expect(commands.some((command) => command[0] === "tmux" && command[1] === "kill-pane")).toBe(
      true
    );
    expect(commands.some((command) => command[1] === "new-window")).toBe(false);
    expect(managedState.controllerLocator).toEqual(locator);
  });

  it("spawns a replacement when a stale controller claim receives a delivery exception", async () => {
    const stateDir = await temporaryDir();
    const state = newLegionState("omp", 1);
    state.roles[controllerToken("omp")] = {
      issue: root,
      role: "controller",
      sessionId: "ses-stale",
    };
    let controllerSpawned = false;
    const { manager: processes, publications } = manager(state, {
      config: config(stateDir),
      run: async (command) => {
        if (command[1] === "list-windows") {
          return {
            stdout: controllerSpawned ? "controller\n" : "",
            exitCode: 0,
          };
        }
        if (command[1] === "list-panes") {
          return controllerSpawned
            ? { stdout: "12345\n", exitCode: 0 }
            : { stdout: "", exitCode: 1 };
        }
        if (command[0] === "kill") return { stdout: "", exitCode: 0 };
        if (command[1] === "new-window") {
          controllerSpawned = true;
          return { stdout: "@42 %1 12345\n", exitCode: 0 };
        }
        return { stdout: "", exitCode: 0 };
      },
    });

    await processes.handleException(
      exception(controllerToken("omp"), {
        topic: "notifications.github.sjawhar.legion.issue.42.comment",
        payload: '{"body":"controller retry"}',
        eventId: "evt-stale-controller",
      })
    );

    expect(controllerSpawned).toBe(true);
    expect(publications).toEqual([]);
  });

  it("marks an exited tree dead and releases its admission slot", async () => {
    const state = newLegionState("omp", 1);
    tree(state);
    state.admission.active.push(root);
    let saves = 0;
    const { manager: processes } = manager(state, {
      saveState: async () => {
        saves += 1;
      },
    });

    await processes.markProcessDead(root);

    expect(state.trees[root]).toMatchObject({ status: "dead" });
    expect(state.admission.active).toEqual([]);
    expect(saves).toBeGreaterThan(0);
  });

  it("ignores a stale explicit process-exit generation", async () => {
    const state = newLegionState("omp", 1);
    tree(state, root, 2);
    state.admission.active.push(root);
    const { manager: processes } = manager(state);

    await processes.markProcessDead(root, 1);

    expect(state.trees[root].status).toBe("active");
    expect(state.admission.active).toEqual([root]);
  });

  it("reclaims a live root architect directly instead of resurrecting it", async () => {
    const state = newLegionState("omp", 1);
    tree(state);
    const { manager: processes, publications } = manager(state, {
      run: liveRun,
    });

    await processes.handleException(exception(roleToken("omp", root, "architect")));

    const original = exception(roleToken("omp", root, "architect")).original;
    expect(publications).toEqual([{ subject: original.topic, json: original.payload }]);
  });

  it("resurrects a dead root architect", async () => {
    const stateDir = await temporaryDir();
    const sessionFile = path.join(stateDir, "architect-session.json");
    await writeFile(sessionFile, "{}", "utf8");
    const state = newLegionState("omp", 1);
    tree(state);
    const rootLocator = state.trees[root].locator;
    if (!rootLocator) throw new Error("test root is missing a locator");
    state.trees[root].locator = { ...rootLocator, ompSessionFile: sessionFile };
    let launched = false;
    const { manager: processes } = manager(state, {
      config: config(stateDir),
      run: async (command) => {
        if (command[1] === "list-windows") return { stdout: "", exitCode: 1 };
        if (command[1] === "new-window") {
          launched = true;
          return { stdout: "@42 %1 12345\n", exitCode: 0 };
        }
        if (command[1] === "split-window") {
          launched = true;
          return { stdout: "%2 12345\n", exitCode: 0 };
        }
        if (command[1] === "list-panes" && command.includes("#{pane_id}")) {
          return launched ? { stdout: "%1\n", exitCode: 0 } : { stdout: "", exitCode: 1 };
        }
        if (command[1] === "list-panes" && command.includes("#{pane_pid}")) {
          return launched ? { stdout: "12345\n", exitCode: 0 } : { stdout: "", exitCode: 1 };
        }
        return { stdout: "", exitCode: 0 };
      },
    });

    await processes.handleException(exception(roleToken("omp", root, "architect")));

    expect(state.trees[root]).toMatchObject({ generation: 2 });
  });

  it("connects a worker-shim client to the root architect's socket when its tree becomes ready", async () => {
    const state = newLegionState("omp", 1);
    tree(state);
    state.trees[root].locator = {
      ...state.trees[root].locator,
      tmuxSession: "legion-omp",
      tmuxWindowId: "@42",
      socketPath: "/state/workers/sjawhar__legion-42-architect.sock",
    };
    const connectedSockets: string[] = [];
    const client = fakeWorkerRpcClient();
    const { manager: processes } = manager(state, {
      connectWorkerRpc: async (socketPath) => {
        connectedSockets.push(socketPath);
        return client;
      },
    });

    await processes.markTreeReady(root);

    expect(connectedSockets).toEqual(["/state/workers/sjawhar__legion-42-architect.sock"]);
    expect(client.negotiated).toBe(true);
  });

  it("connects a worker-shim client to the controller's socket when it becomes ready", async () => {
    const state = newLegionState("omp", 1);
    state.controllerLocator = {
      tmuxSession: "legion-omp",
      tmuxWindowId: "@43",
      tmuxPaneId: "%1",
      socketPath: "/state/workers/controller.sock",
    };
    const connectedSockets: string[] = [];
    const client = fakeWorkerRpcClient();
    const { manager: processes } = manager(state, {
      connectWorkerRpc: async (socketPath) => {
        connectedSockets.push(socketPath);
        return client;
      },
    });

    await processes.markControllerReady();

    expect(connectedSockets).toEqual(["/state/workers/controller.sock"]);
    expect(client.negotiated).toBe(true);
  });

  it("does nothing when the controller has no recorded socket yet", async () => {
    const state = newLegionState("omp", 1);
    const connectedSockets: string[] = [];
    const { manager: processes } = manager(state, {
      connectWorkerRpc: async (socketPath) => {
        connectedSockets.push(socketPath);
        return fakeWorkerRpcClient();
      },
    });

    await processes.markControllerReady();

    expect(connectedSockets).toEqual([]);
  });

  it("resumes a sub-architect (a child issue's architect role) through the same worker path, not the root-architect resurrection path", async () => {
    const state = newLegionState("omp", 1);
    tree(state);
    state.issues[child] = {
      key: child,
      title: "Child",
      status: "in_progress",
      parent: root,
      children: [],
    };
    const role: LegionRole = "architect";
    const token = roleToken("omp", child, role);
    state.roles[token] = {
      issue: child,
      role,
      sessionId: "ses_sub_architect",
      generation: 1,
      locator: {
        tmuxSession: "legion-omp",
        tmuxWindowId: "@42",
        tmuxPaneId: "%2",
        socketPath: "/state/workers/child-architect.sock",
      },
    };
    const client = fakeWorkerRpcClient();
    // Only an idle client is prompted directly (see `WorkerAdmission.resumeOrQueueExisting`); a
    // real client's own `getState()` call just before this would already have seeded `runState`
    // from `isStreaming` as idle here, since nothing is in flight to interrupt.
    client.setRunStateSilently("idle");
    const {
      manager: processes,
      publications,
      controlRequests,
    } = manager(state, {
      connectWorkerRpc: async () => client,
    });

    await processes.handleException(exception(token));

    expect(client.prompts).toEqual([JSON.stringify({ type: "catchup-worker", unhandled: [] })]);
    expect(publications).toEqual([]);
    expect(controlRequests).toEqual([]);
  });

  // Requires a real tmux installation to prove window IDs survive cosmetic-name collisions.
  it.skipIf(process.env.LEGION_TMUX_LIVE !== "1")(
    "probes its live root through its window id when a duplicate cosmetic name exists",
    async () => {
      const stateDir = await temporaryDir();
      const project = `duplicate${Date.now()}`;
      const state = newLegionState(project, 1);
      state.issues[root] = { key: root, title: "Root", status: "todo", children: [] };
      state.trees[root] = { root, generation: 0, status: "active", launchFailures: 0 };
      state.admission.active.push(root);
      const session = `legion-${project}`;
      const commandRunner = async (command: string[]) => {
        if (command[0] !== "tmux") return { stdout: "", exitCode: 0 };
        const actual =
          command[1] === "new-window" || (command[1] === "new-session" && command.includes("-n"))
            ? [...command.slice(0, -1), "sleep 999"]
            : command;
        const child = Bun.spawn(actual, { stdout: "pipe", stderr: "pipe" });
        const [stdout, stderr, exitCode] = await Promise.all([
          new Response(child.stdout).text(),
          new Response(child.stderr).text(),
          child.exited,
        ]);
        return { stdout: `${stdout}${stderr}`, exitCode };
      };
      const { manager: processes } = manager(state, {
        config: config(stateDir, { legionId: project }),
        readProcessCmdline: async () => "omp\0",
        run: commandRunner,
      });

      try {
        await processes.spawnRoot(root);
        await commandRunner([
          "tmux",
          "new-window",
          "-t",
          session,
          "-n",
          "sjawhar-legion-42",
          "sleep 999",
        ]);

        expect(await processes.probe(root)).toBe("alive");
      } finally {
        await commandRunner(["tmux", "kill-session", "-t", session]);
      }
    }
  );

  // Requires a real tmux installation to ensure session creation has no leftover shell window.
  it.skipIf(process.env.LEGION_TMUX_LIVE !== "1")(
    "creates the first live root without a default bash window",
    async () => {
      const stateDir = await temporaryDir();
      const project = `defaultwindow${Date.now()}`;
      const state = newLegionState(project, 1);
      state.issues[root] = { key: root, title: "Root", status: "todo", children: [] };
      state.trees[root] = { root, generation: 0, status: "active", launchFailures: 0 };
      state.admission.active.push(root);
      const session = `legion-${project}`;
      const commandRunner = async (command: string[]) => {
        if (command[0] !== "tmux") return { stdout: "", exitCode: 0 };
        const actual =
          command[1] === "new-window" || (command[1] === "new-session" && command.includes("-n"))
            ? [...command.slice(0, -1), "sleep 999"]
            : command;
        const child = Bun.spawn(actual, { stdout: "pipe", stderr: "pipe" });
        const [stdout, stderr, exitCode] = await Promise.all([
          new Response(child.stdout).text(),
          new Response(child.stderr).text(),
          child.exited,
        ]);
        return { stdout: `${stdout}${stderr}`, exitCode };
      };
      const { manager: processes } = manager(state, {
        config: config(stateDir, { legionId: project }),
        run: commandRunner,
      });

      try {
        await processes.spawnRoot(root);
        const windows = await commandRunner([
          "tmux",
          "list-windows",
          "-t",
          session,
          "-F",
          "#{window_name}",
        ]);

        expect(windows.stdout.split(/\r?\n/)).not.toContain("bash");
      } finally {
        await commandRunner(["tmux", "kill-session", "-t", session]);
      }
    }
  );

  // Requires a real tmux installation to exercise pane lifecycle.
  it.skipIf(process.env.LEGION_TMUX_LIVE !== "1")(
    "probes a real tmux pane as alive, detects its death, and resurrects it once",
    async () => {
      const stateDir = await temporaryDir();
      const project = `smoke${Date.now()}`;
      const state = newLegionState(project, 1);
      state.issues[root] = { key: root, title: "Root", status: "todo", children: [] };
      state.trees[root] = { root, generation: 0, status: "active", launchFailures: 0 };
      state.admission.active.push(root);
      const session = `legion-${project}`;
      const commandRunner = async (command: string[]) => {
        if (command[0] !== "tmux") return { stdout: "", exitCode: 0 };
        const actual =
          command[1] === "new-window" || (command[1] === "new-session" && command.includes("-n"))
            ? [...command.slice(0, -1), "sleep 999"]
            : command;
        const child = Bun.spawn(actual, { stdout: "pipe", stderr: "pipe" });
        const [stdout, stderr, exitCode] = await Promise.all([
          new Response(child.stdout).text(),
          new Response(child.stderr).text(),
          child.exited,
        ]);
        return { stdout: `${stdout}${stderr}`, exitCode };
      };
      const { manager: processes } = manager(state, {
        config: config(stateDir, { legionId: project }),
        readProcessCmdline: async () => "omp\0",
        run: commandRunner,
      });

      try {
        await processes.spawnRoot(root);
        expect(await processes.probe(root)).toBe("alive");
        const firstWindowId = state.trees[root]?.locator?.tmuxWindowId;
        if (!firstWindowId) throw new Error("live root is missing its tmux window id");
        await commandRunner(["tmux", "kill-pane", "-t", firstWindowId]);
        expect(await processes.probe(root)).toBe("dead");
        await Promise.all([processes.resurrect(root), processes.resurrect(root)]);
        expect(await processes.probe(root)).toBe("alive");
        expect((await commandRunner(["tmux", "list-windows", "-t", session])).stdout).toContain(
          "legion-42"
        );
      } finally {
        await commandRunner(["tmux", "kill-session", "-t", session]);
      }
    }
  );

  it("spawns a worker's first pane as a new window with the full worker env and worker-shim command", async () => {
    const stateDir = await temporaryDir();
    const workspace = path.join(stateDir, "workspaces", "sjawhar", "legion", "legion-42");
    await mkdir(workspace, { recursive: true });
    const state = newLegionState("omp", 1);
    state.issues[root] = {
      key: root,
      title: "Root",
      status: "in_progress",
      children: [],
    };
    state.trees[root] = {
      root,
      generation: 1,
      status: "active",
      launchFailures: 0,
    };
    const commands: string[][] = [];
    const { manager: processes, state: managedState } = manager(state, {
      config: config(stateDir),
      run: async (command) => {
        commands.push(command);
        if (command[0] === "tmux" && command[1] === "new-window") {
          return { stdout: "@99 %201 12345\n", exitCode: 0 };
        }
        return { stdout: "", exitCode: 0 };
      },
    });

    const result = await processes.spawnWorker(root, root, "tester", "verify #41");

    expect(result).toEqual({ status: "spawned", roleToken: roleToken("omp", root, "tester") });
    const windowCommand = commands.find(
      (command) => command[0] === "tmux" && command[1] === "new-window" && command.includes("-n")
    );
    if (!windowCommand) throw new Error("worker spawn did not open a tmux window");
    expect(windowCommand[windowCommand.indexOf("-n") + 1]).toBe("legion-42");
    expect(tmuxWindowEnvironment(windowCommand)).toEqual({
      LEGION_TREE: root,
      LEGION_ISSUE: root,
      LEGION_ROLE: "tester",
      LEGION_WORKSPACE: workspace,
      LEGION_BOOT_TOKEN: "worker-boot-token",
      LEGION_GENERATION: "1",
      LEGION_DAEMON_URL: "http://127.0.0.1:13999",
      LEGION_PROJECT: "omp",
      LEGION_STATE_DIR: stateDir,
      LEGION_CREDENTIAL_HELPER: "!/opt/legion/bun /opt/legion/cli/index.ts credential",
      ENVOY_NATS_URL: "nats://127.0.0.1:4222",
      ENVOY_URL: "http://127.0.0.1:9020",
      GIT_CONFIG_COUNT: "0",
      GIT_TERMINAL_PROMPT: "0",
      PATH: "/full/bin:/usr/bin",
    });
    const promptPath = path.join(
      path.resolve(import.meta.dir, "../../../../pi-envoy"),
      "roles",
      "tester.md"
    );
    expect(windowCommand.at(-1)).toBe(
      `cd ${workspace} && ${process.execPath} ${path.resolve(import.meta.dir, "../../cli/index.ts")} worker-shim --socket ${path.join(stateDir, "workers", "tester-9e2fb104.sock")} -- /opt/oh-my-pi/18.0.3/omp --mode rpc --append-system-prompt "$(cat ${promptPath})" --append-system-prompt '${addressingFragment("omp", root, root, "tester").replaceAll("'", "'\\''")}'`
    );
    const claim = managedState.roles[roleToken("omp", root, "tester")];
    if (!claim || !("issue" in claim)) throw new Error("worker claim was not recorded");
    expect(claim.generation).toBe(1);
    expect(claim.pendingAssignment).toBe("verify #41");
    expect(claim.locator).toMatchObject({
      tmuxSession: "legion-omp",
      tmuxWindowId: "@99",
      tmuxPaneId: "%201",
      socketPath: path.join(stateDir, "workers", "tester-9e2fb104.sock"),
    });
  });

  it("appends a second --append-system-prompt naming the launched process's own role topic and its tree's architect topic", async () => {
    const stateDir = await temporaryDir();
    const workspace = path.join(stateDir, "workspaces", "sjawhar", "legion", "issue-42");
    await mkdir(workspace, { recursive: true });
    const state = newLegionState("omp", 1);
    state.issues[root] = {
      key: root,
      title: "Root",
      status: "in_progress",
      children: [],
    };
    state.trees[root] = {
      root,
      generation: 1,
      status: "active",
      launchFailures: 0,
    };
    const { manager: processes, commands } = manager(state, {
      config: config(stateDir),
      run: async (command) => {
        commands.push(command);
        if (command[0] === "tmux" && command[1] === "new-window") {
          return { stdout: "@99 %201 12345\n", exitCode: 0 };
        }
        return { stdout: "", exitCode: 0 };
      },
    });

    await processes.spawnWorker(root, root, "implementer", "implement #41");

    const workerLaunch = commands.find(
      (command) => command[0] === "tmux" && command[1] === "new-window"
    );
    if (!workerLaunch) throw new Error("worker spawn did not open a tmux window");
    const workerArgv = workerLaunch.at(-1) ?? "";
    expect(workerArgv).toContain(roleTopic(roleToken("omp", root, "implementer")));
    expect(workerArgv).toContain(roleTopic(roleToken("omp", root, "architect")));

    const { manager: rootProcesses, commands: rootCommands } = manager(newLegionState("omp", 1), {
      config: config(stateDir),
    });
    await rootProcesses.spawnRoot(root);

    const rootLaunch = rootCommands.find(
      (command) => command[0] === "tmux" && command[1] === "new-window"
    );
    if (!rootLaunch) throw new Error("root spawn did not open a tmux window");
    const rootArgv = rootLaunch.at(-1) ?? "";
    expect(rootArgv).toContain(roleTopic(roleToken("omp", root, "architect")));
  });

  it("splits a second worker on the same issue into the window the first worker just opened", async () => {
    const stateDir = await temporaryDir();
    const workspace = path.join(stateDir, "workspaces", "sjawhar", "legion", "issue-42");
    await mkdir(workspace, { recursive: true });
    const state = newLegionState("omp", 1);
    state.issues[root] = {
      key: root,
      title: "Root",
      status: "in_progress",
      children: [],
    };
    state.trees[root] = {
      root,
      generation: 1,
      status: "active",
      launchFailures: 0,
    };
    const commands: string[][] = [];
    const { manager: processes } = manager(state, {
      config: config(stateDir),
      run: async (command) => {
        commands.push(command);
        if (command[0] === "tmux" && command[1] === "new-window") {
          return { stdout: "@99 %101 12345\n", exitCode: 0 };
        }
        if (command[0] === "tmux" && command[1] === "split-window") {
          return { stdout: "%201 67890\n", exitCode: 0 };
        }
        if (
          command[0] === "tmux" &&
          command[1] === "list-panes" &&
          command.includes("#{pane_id}")
        ) {
          return { stdout: "%101\n", exitCode: 0 };
        }
        return { stdout: "", exitCode: 0 };
      },
    });

    await processes.spawnWorker(root, root, "planner", "plan #41");
    const result = await processes.spawnWorker(root, root, "tester", "verify #41");

    expect(result.status).toBe("spawned");
    expect(
      commands.filter((command) => command[0] === "tmux" && command[1] === "new-window")
    ).toHaveLength(1);
    const split = commands.find(
      (command) => command[0] === "tmux" && command[1] === "split-window"
    );
    if (!split) throw new Error("second worker did not split the existing window");
    expect(split).toContain("@99");
    expect(commands).toContainEqual(["tmux", "select-layout", "-t", "@99", "tiled"]);
  });

  it("serializes two concurrent first spawns on the same issue into a single new window", async () => {
    const stateDir = await temporaryDir();
    const workspace = path.join(stateDir, "workspaces", "sjawhar", "legion", "issue-42");
    await mkdir(workspace, { recursive: true });
    const state = newLegionState("omp", 1);
    state.issues[root] = {
      key: root,
      title: "Root",
      status: "in_progress",
      children: [],
    };
    state.trees[root] = {
      root,
      generation: 1,
      status: "active",
      launchFailures: 0,
    };
    const commands: string[][] = [];
    let windowsOpened = 0;
    const { manager: processes, state: managedState } = manager(state, {
      config: config(stateDir),
      run: async (command) => {
        commands.push(command);
        if (command[0] === "tmux" && command[1] === "new-window") {
          // Widens the race window a concurrency bug would need to slip through.
          await Bun.sleep(5);
          windowsOpened += 1;
          return { stdout: `@99 %${100 + windowsOpened} 12345\n`, exitCode: 0 };
        }
        if (command[0] === "tmux" && command[1] === "split-window") {
          return { stdout: "%201 67890\n", exitCode: 0 };
        }
        if (
          command[0] === "tmux" &&
          command[1] === "list-panes" &&
          command.includes("#{pane_id}")
        ) {
          return windowsOpened > 0
            ? { stdout: "%101\n", exitCode: 0 }
            : { stdout: "", exitCode: 1 };
        }
        return { stdout: "", exitCode: 0 };
      },
    });

    const [plannerResult, testerResult] = await Promise.all([
      processes.spawnWorker(root, root, "planner", "plan #41"),
      processes.spawnWorker(root, root, "tester", "verify #41"),
    ]);

    expect(plannerResult.status).toBe("spawned");
    expect(testerResult.status).toBe("spawned");
    expect(
      commands.filter((command) => command[0] === "tmux" && command[1] === "new-window")
    ).toHaveLength(1);
    expect(
      commands.filter((command) => command[0] === "tmux" && command[1] === "split-window")
    ).toHaveLength(1);
    const plannerClaim = managedState.roles[roleToken("omp", root, "planner")];
    const testerClaim = managedState.roles[roleToken("omp", root, "tester")];
    if (!plannerClaim || !("issue" in plannerClaim) || !testerClaim || !("issue" in testerClaim)) {
      throw new Error("both worker claims must be recorded");
    }
    expect(plannerClaim.locator?.tmuxWindowId).toBe("@99");
    expect(testerClaim.locator?.tmuxWindowId).toBe("@99");
  });

  it("retires its own just-opened pane and reports TreeClosingError, not a launch failure, when the tree starts closing while the pane was opening", async () => {
    const stateDir = await temporaryDir();
    const workspace = path.join(stateDir, "workspaces", "sjawhar", "legion", "issue-43");
    await mkdir(workspace, { recursive: true });
    const state = newLegionState("omp", 1);
    state.issues[root] = {
      key: root,
      title: "Root",
      status: "in_progress",
      children: [child],
    };
    state.issues[child] = {
      key: child,
      title: "Child",
      status: "in_progress",
      parent: root,
      children: [],
    };
    state.trees[root] = {
      root,
      generation: 1,
      locator: {
        tmuxSession: "legion-omp",
        tmuxWindowId: "@42",
        tmuxPaneId: "%0",
        socketPath: "/state/workers/architect.sock",
      },
      status: "active",
      launchFailures: 0,
    };
    const paneOpenGate = Promise.withResolvers<void>();
    const commands: string[][] = [];
    const stuckRootClient = fakeWorkerRpcClient();
    stuckRootClient.shutdown = () => {};
    const { manager: processes, state: managedState } = manager(state, {
      sleep: async () => {},
      config: config(stateDir),
      connectWorkerRpc: async () => stuckRootClient,
      run: async (command) => {
        commands.push(command);
        if (command[0] === "tmux" && command[1] === "new-window") {
          await paneOpenGate.promise;
          return { stdout: "@99 %201 12345\n", exitCode: 0 };
        }
        if (
          command[0] === "tmux" &&
          command[1] === "list-panes" &&
          command.includes("#{pane_pid}")
        ) {
          return { stdout: "4242\n", exitCode: 0 };
        }
        return { stdout: "", exitCode: 0 };
      },
    });

    const spawnPromise = processes.spawnWorker(root, child, "tester", "verify #41");
    // Poll (real macrotask ticks, not just microtasks -- the workspace/socket prep this crosses
    // first are real fs operations) until the launch has actually reached its blocked
    // `new-window` call before starting the race.
    for (
      let attempt = 0;
      attempt < 100 && !commands.some((c) => c[1] === "new-window");
      attempt++
    ) {
      await new Promise((resolve) => setTimeout(resolve, 1));
    }
    expect(commands.some((c) => c[1] === "new-window")).toBe(true);

    const closePromise = processes.closeTree(root);
    paneOpenGate.resolve();

    const result = await spawnPromise.catch((caught: unknown) => caught);

    expect(result).toBeInstanceOf(TreeClosingError);
    expect(commands).toContainEqual(["tmux", "kill-pane", "-t", "%201"]);
    expect(managedState.roles[roleToken("omp", child, "tester")]).toBeUndefined();

    await closePromise;
  });

  it("keeps closeTree from finishing while a launch is still in flight, so its own retire always runs before closingTrees clears", async () => {
    // Without waiting on `inFlightLaunches`, closeTree's fixed-point loop could see an empty
    // `state.roles` snapshot (the launch below hasn't written its claim yet), finish, and clear
    // `closingTrees` -- all while this launch is still blocked mid-flight. Once it later
    // resumes, its own post-launch closing check would find `closingTrees` already empty and
    // write the claim as if nothing had happened, leaving a fresh pane alive and untracked in a
    // tree already reported closed.
    const stateDir = await temporaryDir();
    const workspace = path.join(stateDir, "workspaces", "sjawhar", "legion", "issue-43");
    await mkdir(workspace, { recursive: true });
    const state = newLegionState("omp", 1);
    state.issues[root] = {
      key: root,
      title: "Root",
      status: "in_progress",
      children: [child],
    };
    state.issues[child] = {
      key: child,
      title: "Child",
      status: "in_progress",
      parent: root,
      children: [],
    };
    // No root locator: the root leg is skipped entirely, isolating this test to the worker race.
    state.trees[root] = {
      root,
      generation: 1,
      status: "active",
      launchFailures: 0,
    };
    const paneOpenGate = Promise.withResolvers<void>();
    const commands: string[][] = [];
    const { manager: processes, state: managedState } = manager(state, {
      sleep: async () => {},
      config: config(stateDir),
      run: async (command) => {
        commands.push(command);
        if (command[0] === "tmux" && command[1] === "new-window") {
          await paneOpenGate.promise;
          return { stdout: "@99 %201 12345\n", exitCode: 0 };
        }
        return { stdout: "", exitCode: 0 };
      },
    });

    const spawnPromise = processes.spawnWorker(root, child, "tester", "verify #41");
    for (
      let attempt = 0;
      attempt < 100 && !commands.some((c) => c[1] === "new-window");
      attempt++
    ) {
      await new Promise((resolve) => setTimeout(resolve, 1));
    }
    expect(commands.some((c) => c[1] === "new-window")).toBe(true);

    let closeSettled = false;
    const closePromise = processes.closeTree(root).then(() => {
      closeSettled = true;
    });

    // Give closeTree every real chance to run its (otherwise-empty) worker loop and finish while
    // the launch above is still blocked on `new-window`.
    await new Promise((resolve) => setTimeout(resolve, 20));
    expect(closeSettled).toBe(false);

    paneOpenGate.resolve();
    const spawnResult = await spawnPromise.catch((caught: unknown) => caught);
    await closePromise;

    expect(spawnResult).toBeInstanceOf(TreeClosingError);
    expect(closeSettled).toBe(true);
    expect(commands).toContainEqual(["tmux", "kill-pane", "-t", "%201"]);
    expect(managedState.roles[roleToken("omp", child, "tester")]).toBeUndefined();
    expect(managedState.trees[root].status).toBe("closed");
  });

  it("preserves a launch's just-opened locator for closeTree to retry when its own post-launch retire fails to stop the pane", async () => {
    const stateDir = await temporaryDir();
    const workspace = path.join(stateDir, "workspaces", "sjawhar", "legion", "issue-43");
    await mkdir(workspace, { recursive: true });
    const state = newLegionState("omp", 1);
    state.issues[root] = {
      key: root,
      title: "Root",
      status: "in_progress",
      children: [child],
    };
    state.issues[child] = {
      key: child,
      title: "Child",
      status: "in_progress",
      parent: root,
      children: [],
    };
    state.trees[root] = {
      root,
      generation: 1,
      status: "active",
      launchFailures: 0,
    };
    const paneOpenGate = Promise.withResolvers<void>();
    const commands: string[][] = [];
    const errorLog = vi.spyOn(console, "error").mockImplementation(() => {});
    const { manager: processes, state: managedState } = manager(state, {
      sleep: async () => {},
      config: config(stateDir),
      connectWorkerRpc: async () => {
        throw new Error("ECONNREFUSED");
      },
      run: async (command) => {
        commands.push(command);
        if (command[0] === "tmux" && command[1] === "new-window") {
          await paneOpenGate.promise;
          return { stdout: "@99 %201 12345\n", exitCode: 0 };
        }
        if (command[0] === "tmux" && command[1] === "kill-pane") {
          return { stdout: "", stderr: "no server running", exitCode: 1 };
        }
        return { stdout: "", exitCode: 0 };
      },
    });

    const spawnPromise = processes.spawnWorker(root, child, "tester", "verify #41");
    for (
      let attempt = 0;
      attempt < 100 && !commands.some((c) => c[1] === "new-window");
      attempt++
    ) {
      await new Promise((resolve) => setTimeout(resolve, 1));
    }
    const closePromise = processes.closeTree(root);
    paneOpenGate.resolve();

    const spawnResult = await spawnPromise.catch((caught: unknown) => caught);
    expect(spawnResult).toBeInstanceOf(StopFailed);
    const token = roleToken("omp", child, "tester");
    const claim = managedState.roles[token];
    if (!claim || !("issue" in claim)) throw new Error("claim was discarded on StopFailed");
    expect(claim.locator?.tmuxPaneId).toBe("%201");
    // This claim never existed before this launch, so freshClaim's carry-over
    // (`claim?.launchFailures ?? 0`) has nothing to carry and starts at 0 — never reset by a
    // mere relaunch; only accumulated across retries and cleared by /worker/started.
    expect(claim.launchFailures).toBe(0);

    const closeResult = await closePromise.catch((caught: unknown) => caught);
    expect(closeResult).toBeInstanceOf(StopFailed);
    expect(managedState.trees[root].status).toBe("lingering");
    errorLog.mockRestore();
  });

  it("throws TreeClosingError from a stale-claim probe's recheck when a concurrent closeTree starts mid-probe, leaving the untouched old claim for closeTree itself to reap", async () => {
    const stateDir = await temporaryDir();
    const workspace = path.join(stateDir, "workspaces", "sjawhar", "legion", "issue-42");
    await mkdir(workspace, { recursive: true });
    const state = newLegionState("omp", 1);
    state.issues[root] = {
      key: root,
      title: "Root",
      status: "in_progress",
      children: [],
    };
    // No root locator: isolates this test to the worker race.
    state.trees[root] = {
      root,
      generation: 1,
      status: "active",
      launchFailures: 0,
    };
    const token = roleToken("omp", root, "tester");
    state.roles[token] = {
      issue: root,
      role: "tester",
      generation: 1,
      sessionId: "ses_tester",
      locator: {
        tmuxSession: "legion-omp",
        tmuxWindowId: "@42",
        tmuxPaneId: "%7",
        socketPath: "/state/workers/tester.sock",
      },
    };
    const probeGate = Promise.withResolvers<void>();
    const stuckClient = fakeWorkerRpcClient();
    stuckClient.getStateImpl = async () => {
      await probeGate.promise;
      throw new Error("dead");
    };
    const commands: string[][] = [];
    const { manager: processes, state: managedState } = manager(state, {
      config: config(stateDir),
      sleep: async () => {},
      connectWorkerRpc: async () => stuckClient,
      run: async (command) => {
        commands.push(command);
        return { stdout: "", exitCode: 0 };
      },
    });

    const spawnPromise = processes.spawnWorker(root, root, "tester", "verify again");
    for (let attempt = 0; attempt < 100 && stuckClient.getStateCalls === 0; attempt++) {
      await new Promise((resolve) => setTimeout(resolve, 1));
    }
    expect(stuckClient.getStateCalls).toBe(1);

    let closeSettled = false;
    const closePromise = processes.closeTree(root).then(() => {
      closeSettled = true;
    });
    // Give closeTree every real chance to reap the untouched stale claim and finish while the
    // probe above is still blocked -- without `inFlightLaunches` covering this whole decision
    // (not merely `launchWorker`'s own pane-opening step), closeTree's fixed-point loop could
    // take its first snapshot right now, see the claim's still-stale locator, stop it, and
    // delete the claim, all before the still-running decision below has had any chance to
    // recheck `closingTrees` and bail out on its own.
    await new Promise((resolve) => setTimeout(resolve, 20));
    expect(closeSettled).toBe(false);

    probeGate.resolve();
    const spawnResult = await spawnPromise.catch((caught: unknown) => caught);
    await closePromise;

    expect(spawnResult).toBeInstanceOf(TreeClosingError);
    expect(closeSettled).toBe(true);
    // Never retired or relaunched: the decision's post-probe recheck threw before either step,
    // leaving the stale claim entirely untouched for closeTree itself to reap.
    expect(
      commands.some((c) => c[0] === "tmux" && (c[1] === "new-window" || c[1] === "split-window"))
    ).toBe(false);
    expect(managedState.roles[token]).toBeUndefined();
    expect(managedState.trees[root].status).toBe("closed");
  });

  it("rewrites every claim's stale window id once a dead recorded window falls back to a fresh one", async () => {
    const stateDir = await temporaryDir();
    const workspace = path.join(stateDir, "workspaces", "sjawhar", "legion", "issue-42");
    await mkdir(workspace, { recursive: true });
    const state = newLegionState("omp", 1);
    state.issues[root] = {
      key: root,
      title: "Root",
      status: "in_progress",
      children: [],
    };
    state.trees[root] = {
      root,
      generation: 1,
      status: "active",
      launchFailures: 0,
    };
    const plannerToken = roleToken("omp", root, "planner");
    const implementerToken = roleToken("omp", root, "implementer");
    state.roles[plannerToken] = {
      issue: root,
      role: "planner",
      generation: 1,
      locator: {
        tmuxSession: "legion-omp",
        tmuxWindowId: "@42",
        tmuxPaneId: "%1",
        socketPath: "/state/workers/planner.sock",
      },
    };
    state.roles[implementerToken] = {
      issue: root,
      role: "implementer",
      generation: 1,
      locator: {
        tmuxSession: "legion-omp",
        tmuxWindowId: "@42",
        tmuxPaneId: "%2",
        socketPath: "/state/workers/implementer.sock",
      },
    };
    const commands: string[][] = [];
    const { manager: processes, state: managedState } = manager(state, {
      config: config(stateDir),
      connectWorkerRpc: async () => {
        throw new Error("ECONNREFUSED");
      },
      run: async (command) => {
        commands.push(command);
        if (command[0] === "tmux" && command[1] === "new-window") {
          return { stdout: "@99 %201 12345\n", exitCode: 0 };
        }
        if (command[0] === "tmux" && command[1] === "split-window") {
          return { stdout: "%301 67890\n", exitCode: 0 };
        }
        if (
          command[0] === "tmux" &&
          command[1] === "list-panes" &&
          command.includes("#{pane_id}")
        ) {
          return command.includes("@42")
            ? { stdout: "", exitCode: 1 }
            : { stdout: "%201\n", exitCode: 0 };
        }
        return { stdout: "", exitCode: 0 };
      },
    });

    await processes.spawnWorker(root, root, "tester", "verify #41");

    expect(
      commands.filter((command) => command[0] === "tmux" && command[1] === "new-window")
    ).toHaveLength(1);
    for (const token of [plannerToken, implementerToken]) {
      const claim = managedState.roles[token];
      if (!claim || !("issue" in claim)) throw new Error(`${token} claim disappeared`);
      expect(claim.locator?.tmuxWindowId).toBe("@99");
    }

    await processes.spawnWorker(root, root, "reviewer", "review #41");

    expect(
      commands.filter((command) => command[0] === "tmux" && command[1] === "new-window")
    ).toHaveLength(1);
    const split = commands.find(
      (command) => command[0] === "tmux" && command[1] === "split-window"
    );
    if (!split) throw new Error("fourth worker did not split into the rewritten window");
    expect(split).toContain("@99");
  });

  it("resumes an already-alive worker by sending the task over its live socket, spawning nothing new, and records the reassignment as the issue's active phase", async () => {
    const state = newLegionState("omp", 1);
    state.issues[root] = {
      key: root,
      title: "Root",
      status: "in_progress",
      children: [],
    };
    tree(state);
    const token = roleToken("omp", root, "tester");
    state.roles[token] = {
      issue: root,
      role: "tester",
      generation: 2,
      sessionId: "ses_tester",
      locator: {
        tmuxSession: "legion-omp",
        tmuxWindowId: "@42",
        tmuxPaneId: "%7",
        socketPath: "/state/workers/tester.sock",
        ompSessionFile: "/state/workers/tester-session.json",
      },
    };
    const client = fakeWorkerRpcClient();
    // Only an idle client is prompted directly (see `WorkerAdmission.resumeOrQueueExisting`); a
    // real client's own `getState()` call just before this would already have seeded `runState`
    // from `isStreaming` as idle here, since nothing is in flight to interrupt.
    client.setRunStateSilently("idle");
    const {
      manager: processes,
      state: managedState,
      commands,
    } = manager(state, {
      connectWorkerRpc: async () => client,
    });

    const result = await processes.spawnWorker(root, root, "tester", "verify #55");

    expect(result).toEqual({ status: "resumed", roleToken: token });
    expect(client.prompts).toEqual(["verify #55"]);
    expect(commands.some((command) => command[0] === "tmux")).toBeFalse();
    // A phase worker resumed for a repeat assignment (the architect requested changes, or
    // reassigned it a second time) must re-register as the issue's active phase, or its
    // eventual `legion handoff complete` 409s forever against a phase no route ever restored.
    expect(managedState.phases[root]).toEqual({ phase: "tester", sessionId: "ses_tester" });
  });

  it("treats a same-role spawn during an in-flight boot as resumed-pending, never launching a second pane", async () => {
    const stateDir = await temporaryDir();
    const workspace = path.join(stateDir, "workspaces", "sjawhar", "legion", "issue-42");
    await mkdir(workspace, { recursive: true });
    const state = newLegionState("omp", 1);
    state.issues[root] = {
      key: root,
      title: "Root",
      status: "in_progress",
      children: [],
    };
    tree(state);
    const token = roleToken("omp", root, "tester");
    const {
      manager: processes,
      state: managedState,
      commands,
    } = manager(state, {
      config: config(stateDir),
    });

    const first = await processes.spawnWorker(root, root, "tester", "verify #41");
    expect(first).toEqual({ status: "spawned", roleToken: token });

    // /worker/started has not run yet, so the claim has a locator but no sessionId: this second
    // call must never open or split another pane, and must queue its task for worker/ready.
    const second = await processes.spawnWorker(root, root, "tester", "verify #55");
    expect(second).toEqual({ status: "resumed", roleToken: token });

    expect(
      commands.filter(
        (command) =>
          command[0] === "tmux" && (command[1] === "new-window" || command[1] === "split-window")
      )
    ).toHaveLength(1);
    const claim = managedState.roles[token];
    if (!claim || !("issue" in claim)) throw new Error("worker claim disappeared");
    expect(claim.sessionId).toBeUndefined();
    expect(claim.pendingAssignment).toBe("verify #55");
  });

  it("respawns with --resume when a worker's claimed socket is dead, splitting into its own persisted window", async () => {
    const stateDir = await temporaryDir();
    const workspace = path.join(stateDir, "workspaces", "sjawhar", "legion", "issue-42");
    await mkdir(workspace, { recursive: true });
    await writeFile(path.join(stateDir, "prior-tester-session.json"), "{}", "utf8");
    const state = newLegionState("omp", 1);
    state.issues[root] = {
      key: root,
      title: "Root",
      status: "in_progress",
      children: [],
    };
    state.trees[root] = {
      root,
      generation: 1,
      status: "active",
      launchFailures: 0,
    };
    const token = roleToken("omp", root, "tester");
    state.roles[token] = {
      issue: root,
      role: "tester",
      generation: 1,
      sessionId: "ses_tester",
      locator: {
        tmuxSession: "legion-omp",
        tmuxWindowId: "@42",
        tmuxPaneId: "%7",
        socketPath: "/state/workers/dead-tester.sock",
        ompSessionFile: path.join(stateDir, "prior-tester-session.json"),
      },
    };
    const commands: string[][] = [];
    const { manager: processes, state: managedState } = manager(state, {
      config: config(stateDir),
      connectWorkerRpc: async () => {
        throw new Error("ECONNREFUSED");
      },
      run: async (command) => {
        commands.push(command);
        if (command[0] === "tmux" && command[1] === "split-window") {
          return { stdout: "%301 23456\n", exitCode: 0 };
        }
        if (
          command[0] === "tmux" &&
          command[1] === "list-panes" &&
          command.includes("#{pane_id}")
        ) {
          return { stdout: "%301\n", exitCode: 0 };
        }
        return { stdout: "", exitCode: 0 };
      },
    });

    const result = await processes.spawnWorker(root, root, "tester", "verify again");

    expect(result).toEqual({ status: "spawned", roleToken: token });
    const split = commands.find(
      (command) => command[0] === "tmux" && command[1] === "split-window"
    );
    if (!split) throw new Error("dead-worker respawn did not split its persisted window");
    expect(split).toContain("@42");
    expect(split.at(-1)).toContain(`--resume=${path.join(stateDir, "prior-tester-session.json")}`);
    const claim = managedState.roles[token];
    if (!claim || !("issue" in claim)) throw new Error("worker claim was not recorded");
    expect(claim.generation).toBe(2);
  });

  it("queues a dead worker retried at cap by preserving its resume session file, then promotes it with --resume once a slot frees", async () => {
    const stateDir = await temporaryDir();
    const workspace = path.join(stateDir, "workspaces", "sjawhar", "legion", "issue-42");
    await mkdir(workspace, { recursive: true });
    const resumeFile = path.join(stateDir, "prior-tester-session.json");
    await writeFile(resumeFile, "{}", "utf8");
    const state = newLegionState("omp", 1);
    tree(state);
    const plannerToken = roleToken("omp", root, "planner");
    const testerToken = roleToken("omp", root, "tester");
    const occupierClient = fakeWorkerRpcClient();
    // Seeded idle so planner's own resume below prompts directly (see
    // `WorkerAdmission.resumeOrQueueExisting`), occupying the cap-1 slot: `prompt()` itself
    // then flips it to "running", matching what actually happens once the daemon calls it.
    occupierClient.setRunStateSilently("idle");
    state.roles[plannerToken] = {
      issue: root,
      role: "planner",
      generation: 1,
      sessionId: "ses_planner",
      readyConfirmedAt: Date.parse("2026-08-24T00:00:00.000Z"),
      locator: {
        tmuxSession: "legion-omp",
        tmuxWindowId: "@42",
        tmuxPaneId: "%1",
        socketPath: "/state/workers/planner.sock",
      },
    };
    state.roles[testerToken] = {
      issue: root,
      role: "tester",
      generation: 1,
      sessionId: "ses_tester",
      readyConfirmedAt: Date.parse("2026-08-24T00:00:00.000Z"),
      agentId: "agt_tester",
      locator: {
        tmuxSession: "legion-omp",
        tmuxWindowId: "@42",
        tmuxPaneId: "%7",
        socketPath: "/state/workers/dead-tester.sock",
        ompSessionFile: resumeFile,
      },
    };
    let resolvePromoted: (() => void) | undefined;
    const promoted = new Promise<void>((resolve) => {
      resolvePromoted = resolve;
    });
    let promotedExpectedSessionId: string | undefined;
    const {
      manager: processes,
      state: managedState,
      commands,
    } = manager(state, {
      config: config(stateDir, { workerCap: 1 }),
      sleep: async () => {},
      connectWorkerRpc: async (socketPath) => {
        if (socketPath === "/state/workers/dead-tester.sock") throw new Error("ECONNREFUSED");
        return occupierClient;
      },
      mintWorkerBootToken: async (_tree, _issue, _role, _generation, expectedSessionId) => {
        promotedExpectedSessionId = expectedSessionId;
        return "worker-boot-token";
      },
      saveState: async () => {
        const testerClaim = managedState.roles[testerToken];
        // The fixture's tester claim already carries a (stale, dead-socket) locator from the
        // start, so a bare `locator` truthiness check would resolve `promoted` prematurely
        // during the planner's own earlier resume `saveState` call. `%301` is the pane id only
        // the fresh promoted-relaunch's split-window mock reports, so this fires exactly once,
        // after the actual promotion lands.
        if (testerClaim && "issue" in testerClaim && testerClaim.locator?.tmuxPaneId === "%301") {
          resolvePromoted?.();
        }
      },
      run: async (command) => {
        commands.push(command);
        if (command[0] === "tmux" && command[1] === "list-panes" && command.includes("%7")) {
          return { stdout: "", exitCode: 1 };
        }
        if (command[0] === "tmux" && command[1] === "split-window") {
          return { stdout: "%301 23456\n", exitCode: 0 };
        }
        if (
          command[0] === "tmux" &&
          command[1] === "list-panes" &&
          command.includes("#{pane_id}")
        ) {
          return { stdout: "%301\n", exitCode: 0 };
        }
        return { stdout: "", exitCode: 0 };
      },
    });

    // Discovers tester's dead socket up front (clearing its locator, preserving
    // resumeSessionFile) exactly as a real daemon restart's own reconnect sweep would - without
    // this, tester's still-recorded (but never yet probed) locator conservatively counts
    // against the cap-1 slot planner is about to claim (see `runningWorkerCount`'s doc
    // comment), blocking planner's own resume below before it ever gets a turn.
    await processes.reconnectWorkers();

    // Bring the planner into the cache as running, occupying the cap-1 slot.
    const plannerResume = await processes.spawnWorker(root, root, "planner", "plan #41");
    expect(plannerResume).toEqual({ status: "resumed", roleToken: plannerToken });
    expect(occupierClient.prompts).toEqual(["plan #41"]);

    const testerResult = await processes.spawnWorker(root, root, "tester", "verify again");

    expect(testerResult).toEqual({ status: "queued", roleToken: testerToken });
    expect(managedState.workerAdmission.queue).toEqual([testerToken]);
    const queuedClaim = managedState.roles[testerToken];
    if (!queuedClaim || !("issue" in queuedClaim)) throw new Error("tester claim disappeared");
    expect(queuedClaim.locator).toBeUndefined();
    expect(queuedClaim.resumeSessionFile).toBe(resumeFile);
    expect(queuedClaim.pendingAssignment).toBe("verify again");
    // Preserved in place, never replaced.
    expect(queuedClaim.agentId).toBe("agt_tester");
    // sessionId is kept (never deleted) while queued: a delayed promotion's boot token still
    // names this exact session as the one it must resume — the "respawn must resume the same
    // agent session" 409 guard the daemon-side worker/started handler enforces depends on it.
    expect(queuedClaim.sessionId).toBe("ses_tester");

    occupierClient.emitRunState("idle");
    await promoted;

    const promotedClaim = managedState.roles[testerToken];
    if (!promotedClaim || !("issue" in promotedClaim)) throw new Error("tester claim disappeared");
    expect(managedState.workerAdmission.queue).toEqual([]);
    expect(promotedClaim.resumeSessionFile).toBeUndefined();
    // The delayed promotion's mintWorkerBootToken call named the original session as the one
    // it must resume, since sessionId survived the whole queued interval.
    expect(promotedExpectedSessionId).toBe("ses_tester");
    const split = commands.find(
      (command) => command[0] === "tmux" && command[1] === "split-window"
    );
    if (!split) throw new Error("promoted worker did not split into its persisted window");
    expect(split).toContain("@42");
    expect(split.at(-1)).toContain(`--resume=${resumeFile}`);
  });

  it("kills a dead-socket worker's still-running pane before respawning it", async () => {
    const stateDir = await temporaryDir();
    const workspace = path.join(stateDir, "workspaces", "sjawhar", "legion", "issue-42");
    await mkdir(workspace, { recursive: true });
    await writeFile(path.join(stateDir, "prior-tester-session.json"), "{}", "utf8");
    const state = newLegionState("omp", 1);
    state.issues[root] = {
      key: root,
      title: "Root",
      status: "in_progress",
      children: [],
    };
    state.trees[root] = {
      root,
      generation: 1,
      status: "active",
      launchFailures: 0,
    };
    const token = roleToken("omp", root, "tester");
    state.roles[token] = {
      issue: root,
      role: "tester",
      generation: 1,
      sessionId: "ses_tester",
      locator: {
        tmuxSession: "legion-omp",
        tmuxWindowId: "@42",
        tmuxPaneId: "%7",
        socketPath: "/state/workers/dead-tester.sock",
        ompSessionFile: path.join(stateDir, "prior-tester-session.json"),
      },
    };
    const commands: string[][] = [];
    const { manager: processes, state: managedState } = manager(state, {
      config: config(stateDir),
      sleep: async () => {},
      connectWorkerRpc: async () => {
        throw new Error("ECONNREFUSED");
      },
      run: async (command) => {
        commands.push(command);
        if (
          command[0] === "tmux" &&
          command[1] === "list-panes" &&
          command.includes("%7") &&
          command.includes("#{pane_pid}")
        ) {
          // The old pane's OMP child is still running despite the dead socket.
          return { stdout: "22222\n", exitCode: 0 };
        }
        if (command[0] === "tmux" && command[1] === "kill-pane") {
          return { stdout: "", exitCode: 0 };
        }
        if (command[0] === "tmux" && command[1] === "split-window") {
          return { stdout: "%301 23456\n", exitCode: 0 };
        }
        if (
          command[0] === "tmux" &&
          command[1] === "list-panes" &&
          command.includes("#{pane_id}")
        ) {
          return { stdout: "%301\n", exitCode: 0 };
        }
        return { stdout: "", exitCode: 0 };
      },
    });

    const result = await processes.spawnWorker(root, root, "tester", "verify again");

    expect(result).toEqual({ status: "spawned", roleToken: token });
    expect(
      commands.some(
        (command) => command[0] === "tmux" && command[1] === "kill-pane" && command.includes("%7")
      )
    ).toBeTrue();
    const killPaneIndex = commands.findIndex(
      (command) => command[1] === "kill-pane" && command.includes("%7")
    );
    const splitWindowIndex = commands.findIndex((command) => command[1] === "split-window");
    expect(killPaneIndex).toBeGreaterThanOrEqual(0);
    expect(splitWindowIndex).toBeGreaterThan(killPaneIndex);
    const claim = managedState.roles[token];
    if (!claim || !("issue" in claim)) throw new Error("worker claim was not recorded");
    expect(claim.generation).toBe(2);
  });

  it("fails a worker respawn loudly when its recorded OMP session file is missing, never starting fresh", async () => {
    const stateDir = await temporaryDir();
    const workspace = path.join(stateDir, "workspaces", "sjawhar", "legion", "issue-42");
    await mkdir(workspace, { recursive: true });
    const state = newLegionState("omp", 1);
    state.issues[root] = {
      key: root,
      title: "Root",
      status: "in_progress",
      children: [],
    };
    state.trees[root] = {
      root,
      generation: 1,
      status: "active",
      launchFailures: 0,
    };
    const token = roleToken("omp", root, "tester");
    state.roles[token] = {
      issue: root,
      role: "tester",
      generation: 1,
      sessionId: "ses_tester",
      locator: {
        tmuxSession: "legion-omp",
        tmuxWindowId: "@42",
        tmuxPaneId: "%7",
        socketPath: "/state/workers/dead-tester.sock",
        ompSessionFile: path.join(stateDir, "missing-tester-session.json"),
      },
    };
    const commands: string[][] = [];
    const { manager: processes, state: managedState } = manager(state, {
      config: config(stateDir),
      connectWorkerRpc: async () => {
        throw new Error("ECONNREFUSED");
      },
      run: async (command) => {
        commands.push(command);
        return { stdout: "", exitCode: 0 };
      },
    });

    await expect(processes.spawnWorker(root, root, "tester", "verify again")).rejects.toThrow(
      /recorded OMP session file is missing/
    );

    expect(
      commands.some((command) => command[0] === "tmux" && command[1] === "split-window")
    ).toBeFalse();
    const claim = managedState.roles[token];
    if (!claim || !("issue" in claim)) throw new Error("worker claim disappeared");
    expect(claim.launchFailures).toBe(1);
  });

  it("carries launchFailures forward through a successful launch - only a confirmed /worker/started resets it", async () => {
    const stateDir = await temporaryDir();
    const workspace = path.join(stateDir, "workspaces", "sjawhar", "legion", "issue-42");
    await mkdir(workspace, { recursive: true });
    const state = newLegionState("omp", 1);
    state.issues[root] = {
      key: root,
      title: "Root",
      status: "in_progress",
      children: [],
    };
    state.trees[root] = {
      root,
      generation: 1,
      status: "active",
      launchFailures: 0,
    };
    const token = roleToken("omp", root, "tester");
    // A claim that already failed twice below MAX_LAUNCH_FAILURES (e.g. rotated through the
    // queue) but has no locator (this attempt starts clean, not a --resume).
    state.roles[token] = { issue: root, role: "tester", launchFailures: 2 };
    const { manager: processes, state: managedState } = manager(state, {
      config: config(stateDir),
      run: async (command) => {
        if (command[0] === "tmux" && command[1] === "new-window") {
          return { stdout: "@42 %1 12345\n", exitCode: 0 };
        }
        return { stdout: "", exitCode: 0 };
      },
    });

    const result = await processes.spawnWorker(root, root, "tester", "verify again");

    expect(result).toEqual({ status: "spawned", roleToken: token });
    const claim = managedState.roles[token];
    if (!claim || !("issue" in claim)) throw new Error("worker claim was not recorded");
    // A launch opening a pane is not yet a confirmed boot - the watchdog's own accounting
    // (armed by this same launch) must still see the prior failure count in case this pane
    // never gets to /worker/started either. Only that confirmation (api/routes/workers.ts)
    // resets it.
    expect(claim.launchFailures).toBe(2);
  });

  it("respawns a dead-socket worker without deadlocking its own launch queue while retiring the stale locator", async () => {
    // `retireWorkerLocator`'s stop must be the RAW (unserialized) `stopProcess`. This whole call
    // is already running inside `spawnWorker`'s `workerAdmission.mutateClaim(token, …)` callback
    // for this exact token; if `retireWorkerLocator` instead called `stopProcessSerialized`
    // (which re-enters that same per-token critical section), it would await a promise that can
    // only settle after this very callback returns — deadlocking forever.
    const stateDir = await temporaryDir();
    const workspace = path.join(stateDir, "workspaces", "sjawhar", "legion", "issue-42");
    await mkdir(workspace, { recursive: true });
    const state = newLegionState("omp", 1);
    state.issues[root] = {
      key: root,
      title: "Root",
      status: "in_progress",
      children: [],
    };
    state.trees[root] = {
      root,
      generation: 1,
      status: "active",
      launchFailures: 0,
    };
    const token = roleToken("omp", root, "tester");
    state.roles[token] = {
      issue: root,
      role: "tester",
      generation: 1,
      sessionId: "ses_tester",
      locator: {
        tmuxSession: "legion-omp",
        tmuxWindowId: "@42",
        tmuxPaneId: "%7",
        socketPath: "/state/workers/dead-tester.sock",
      },
    };
    const shutdownCalls: string[] = [];
    const staleClient = fakeWorkerRpcClient();
    const shutdown = staleClient.shutdown.bind(staleClient);
    staleClient.shutdown = () => {
      shutdownCalls.push("dead-tester");
      shutdown();
    };
    let connectAttempts = 0;
    // The first connect attempt is `spawnWorker`'s own liveness probe (`probeWorkerSocket` via
    // the cached, negotiating `workerClient`) — a genuine dead socket (connect failure), not
    // merely a busy one that answers `get_state` late (a connected-but-slow client now queues
    // instead of retiring+respawning — see `spawnWorker`'s liveness dialect). The second is
    // `retireWorkerLocator`'s own raw, non-negotiating `stopClient` connect, used only to send
    // the shutdown frame — succeeding here is what this test's `shutdownCalls` assertion needs.
    const { manager: processes } = manager(state, {
      config: config(stateDir),
      connectWorkerRpc: async () => {
        connectAttempts += 1;
        if (connectAttempts === 1) throw new Error("ECONNREFUSED");
        return staleClient;
      },
      run: async (command) => {
        if (command[0] === "tmux" && command[1] === "split-window") {
          return { stdout: "%301 23456\n", exitCode: 0 };
        }
        if (
          command[0] === "tmux" &&
          command[1] === "list-panes" &&
          command.includes("#{pane_id}")
        ) {
          return { stdout: "%7\n", exitCode: 0 };
        }
        return { stdout: "", exitCode: 0 };
      },
    });

    const timeout = Symbol("timeout");
    const result = await Promise.race([
      processes.spawnWorker(root, root, "tester", "verify again"),
      new Promise((resolve) => setTimeout(() => resolve(timeout), 2_000)),
    ]);

    expect(result).not.toBe(timeout);
    expect(result).toEqual({ status: "spawned", roleToken: token });
    expect(shutdownCalls).toEqual(["dead-tester"]);
  });

  it("passes a respawned claim's existing sessionId as the worker boot token's expected session", async () => {
    const stateDir = await temporaryDir();
    const workspace = path.join(stateDir, "workspaces", "sjawhar", "legion", "issue-42");
    await mkdir(workspace, { recursive: true });
    const state = newLegionState("omp", 1);
    state.issues[root] = {
      key: root,
      title: "Root",
      status: "in_progress",
      children: [],
    };
    state.trees[root] = {
      root,
      generation: 1,
      status: "active",
      launchFailures: 0,
    };
    const token = roleToken("omp", root, "tester");
    state.roles[token] = {
      issue: root,
      role: "tester",
      sessionId: "ses_original",
      generation: 1,
      locator: {
        tmuxSession: "legion-omp",
        tmuxWindowId: "@42",
        tmuxPaneId: "%7",
        socketPath: "/state/workers/tester.sock",
      },
    };
    let expectedSessionId: string | undefined;
    const { manager: processes } = manager(state, {
      config: config(stateDir),
      connectWorkerRpc: async () => {
        throw new Error("ECONNREFUSED");
      },
      mintWorkerBootToken: async (_tree, _issue, _role, generation, sessionId) => {
        expectedSessionId = sessionId;
        return `boot-${generation}`;
      },
      run: async (command) => {
        if (command[0] === "tmux" && command[1] === "split-window") {
          return { stdout: "%301 23456\n", exitCode: 0 };
        }
        if (
          command[0] === "tmux" &&
          command[1] === "list-panes" &&
          command.includes("#{pane_id}")
        ) {
          return { stdout: "%7\n", exitCode: 0 };
        }
        return { stdout: "", exitCode: 0 };
      },
    });

    await processes.spawnWorker(root, root, "tester", "verify again");

    expect(expectedSessionId).toBe("ses_original");
  });

  it("refuses to spawn a sub-architect at or beyond the configured recursion depth", async () => {
    const state = newLegionState("omp", 1);
    state.issues[root] = {
      key: root,
      title: "Root",
      status: "in_progress",
      children: [child],
    };
    state.issues[child] = {
      key: child,
      title: "Child",
      parent: root,
      status: "in_progress",
      children: [],
    };
    tree(state);
    const { manager: processes } = manager(state, {
      config: config("/state", { maxRecursionDepth: 1 }),
    });

    await expect(processes.spawnWorker(root, child, "architect", "plan sub-tree")).rejects.toThrow(
      /recursion/
    );
  });

  it("queues a second spawnWorker call at the running-worker cap and publishes worker-queued to the architect", async () => {
    const stateDir = await temporaryDir();
    const state = newLegionState("omp", 1);
    tree(state);
    const {
      manager: processes,
      state: managedState,
      publications,
    } = manager(state, {
      config: config(stateDir, { workerCap: 1 }),
    });

    const first = await processes.spawnWorker(root, root, "planner", "plan #41");
    expect(first).toEqual({ status: "spawned", roleToken: roleToken("omp", root, "planner") });

    const testerToken = roleToken("omp", root, "tester");
    const second = await processes.spawnWorker(root, root, "tester", "verify #41");

    expect(second).toEqual({ status: "queued", roleToken: testerToken });
    expect(managedState.workerAdmission.queue).toEqual([testerToken]);
    const queuedClaim = managedState.roles[testerToken];
    if (!queuedClaim || !("issue" in queuedClaim)) throw new Error("queued claim missing");
    expect(queuedClaim.locator).toBeUndefined();
    expect(queuedClaim.pendingAssignment).toBe("verify #41");
    expect(publications).toContainEqual({
      subject: roleTopic(roleToken("omp", root, "architect")),
      json: JSON.stringify({ type: "worker-queued", issue: root, role: "tester" }),
    });
  });

  it("promotes the queued worker once the running one goes idle, publishing worker-started", async () => {
    const stateDir = await temporaryDir();
    const state = newLegionState("omp", 1);
    tree(state);
    const clients: Array<{ emitRunState(state: "running" | "idle"): void; close(): void }> = [];
    const publications: Array<{ subject: string; json: string }> = [];
    let resolvePromoted: (() => void) | undefined;
    const promoted = new Promise<void>((resolve) => {
      resolvePromoted = resolve;
    });
    const plannerToken = roleToken("omp", root, "planner");
    const testerToken = roleToken("omp", root, "tester");
    const architectTopic = roleTopic(roleToken("omp", root, "architect"));
    const { manager: processes, state: managedState } = manager(state, {
      config: config(stateDir, { workerCap: 1 }),
      connectWorkerRpc: async () => {
        const client = fakeWorkerRpcClient();
        clients.push(client);
        return client;
      },
      natsPublish: (subject, json) => {
        publications.push({ subject, json });
        if (subject === architectTopic && json.includes("worker-started")) resolvePromoted?.();
      },
    });

    await processes.spawnWorker(root, root, "planner", "plan #41");
    await processes.spawnWorker(root, root, "tester", "verify #41");
    expect(managedState.workerAdmission.queue).toEqual([testerToken]);

    const plannerClaim = managedState.roles[plannerToken];
    if (!plannerClaim || !("issue" in plannerClaim)) throw new Error("planner claim missing");
    plannerClaim.sessionId = "ses_planner";
    await processes.workerReady(root, "planner", "ses_planner", plannerClaim.generation ?? 1);
    expect(clients).toHaveLength(1);

    clients[0]?.emitRunState("idle");
    await promoted;

    expect(managedState.workerAdmission.queue).toEqual([]);
    const promotedClaim = managedState.roles[testerToken];
    if (!promotedClaim || !("issue" in promotedClaim)) throw new Error("tester claim missing");
    expect(promotedClaim.locator).toBeDefined();
    expect(publications).toContainEqual({
      subject: architectTopic,
      json: JSON.stringify({ type: "worker-started", issue: root, role: "tester" }),
    });
  });

  it("queues an idle-resume request at the running-worker cap, then promotes it by prompting in place (not relaunching)", async () => {
    const stateDir = await temporaryDir();
    const state = newLegionState("omp", 1);
    tree(state);
    const plannerToken = roleToken("omp", root, "planner");
    const testerToken = roleToken("omp", root, "tester");
    const architectTopic = roleTopic(roleToken("omp", root, "architect"));
    const plannerClient = fakeWorkerRpcClient();
    // Seeded idle so planner's own resume below prompts directly (see
    // `WorkerAdmission.resumeOrQueueExisting`), occupying the cap-1 slot: `prompt()` itself
    // then flips it to "running", matching what actually happens once the daemon calls it.
    plannerClient.setRunStateSilently("idle");
    const testerClient = fakeWorkerRpcClient();
    // Seeded (and, below, connected via `reconnectWorkers`) idle up front too: otherwise
    // tester's still-uncached client conservatively counts against the cap-1 slot planner is
    // about to claim (see `runningWorkerCount`'s doc comment), blocking planner's own resume
    // below before it ever gets a turn.
    testerClient.setRunStateSilently("idle");
    state.roles[plannerToken] = {
      issue: root,
      role: "planner",
      generation: 1,
      sessionId: "ses_planner",
      locator: {
        tmuxSession: "legion-omp",
        tmuxWindowId: "@42",
        tmuxPaneId: "%1",
        socketPath: "/state/workers/planner.sock",
      },
    };
    state.roles[testerToken] = {
      issue: root,
      role: "tester",
      generation: 1,
      sessionId: "ses_tester",
      readyConfirmedAt: Date.parse("2026-08-24T00:00:00.000Z"),
      locator: {
        tmuxSession: "legion-omp",
        tmuxWindowId: "@42",
        tmuxPaneId: "%7",
        socketPath: "/state/workers/tester.sock",
      },
    };
    const publications: Array<{ subject: string; json: string }> = [];
    let resolvePromoted: (() => void) | undefined;
    const promoted = new Promise<void>((resolve) => {
      resolvePromoted = resolve;
    });
    const commands: string[][] = [];
    const { manager: processes, state: managedState } = manager(state, {
      config: config(stateDir, { workerCap: 1 }),
      connectWorkerRpc: async (socketPath) =>
        socketPath === "/state/workers/planner.sock" ? plannerClient : testerClient,
      natsPublish: (subject, json) => {
        publications.push({ subject, json });
        if (subject === architectTopic && json.includes("worker-started")) resolvePromoted?.();
      },
      run: async (command) => {
        commands.push(command);
        return { stdout: "", exitCode: 0 };
      },
    });

    await processes.reconnectWorkers();

    // Bring planner into the cache as RUNNING, occupying the cap-1 slot.
    const plannerResume = await processes.spawnWorker(root, root, "planner", "plan #41");
    expect(plannerResume).toEqual({ status: "resumed", roleToken: plannerToken });
    plannerClient.emitRunState("running");

    // Tester's client is already live and idle (it just finished a previous turn) — but the
    // cap has no free slot, so prompting it immediately would flip an uncounted-idle worker to
    // running past config.workerCap. It must queue instead, leaving the live pane and client
    // completely alone.
    testerClient.emitRunState("idle");
    const testerResume = await processes.spawnWorker(root, root, "tester", "verify #41");

    expect(testerResume).toEqual({ status: "queued", roleToken: testerToken });
    expect(testerClient.prompts).toEqual([]);
    expect(managedState.workerAdmission.queue).toEqual([testerToken]);
    const queuedClaim = managedState.roles[testerToken];
    if (!queuedClaim || !("issue" in queuedClaim)) throw new Error("tester claim disappeared");
    // Locator left completely alone: no relaunch is ever needed for a still-live idle pane.
    expect(queuedClaim.locator).toBeDefined();
    expect(queuedClaim.pendingAssignment).toBe("verify #41");
    expect(publications).toContainEqual({
      subject: architectTopic,
      json: JSON.stringify({ type: "worker-queued", issue: root, role: "tester" }),
    });

    // Planner frees the slot; tester's already-idle, already-cached client is prompted in
    // place — no new tmux command is ever issued for it.
    plannerClient.emitRunState("idle");
    await promoted;

    expect(managedState.workerAdmission.queue).toEqual([]);
    expect(testerClient.prompts).toEqual(["verify #41"]);
    expect(
      commands.some((command) => command[1] === "split-window" || command[1] === "new-window")
    ).toBeFalse();
    const promotedClaim = managedState.roles[testerToken];
    if (!promotedClaim || !("issue" in promotedClaim)) throw new Error("tester claim missing");
    expect(promotedClaim.pendingAssignment).toBeUndefined();
    // The promoted-by-prompt path must register the phase exactly as a fresh launch or a
    // direct /worker/ready resume does — otherwise phase/complete 409s forever for a worker
    // that was promoted this way.
    expect(managedState.phases[root]).toEqual({ phase: "tester", sessionId: "ses_tester" });
    expect(publications).toContainEqual({
      subject: architectTopic,
      json: JSON.stringify({ type: "worker-started", issue: root, role: "tester" }),
    });
  });

  it("leaves a queued idle-resume assignment intact and stops draining when the promotion prompt itself rejects", async () => {
    const token = roleToken("omp", root, "tester");
    const client = fakeWorkerRpcClient();
    client.emitRunState("idle");
    client.prompt = async () => {
      throw new Error("shim write failed");
    };
    const { processes, state, managedState } = await workerCapFixture(1, {
      connectWorkerRpc: async () => client,
    });
    state.roles[token] = {
      issue: root,
      role: "tester",
      generation: 1,
      sessionId: "ses_tester",
      readyConfirmedAt: Date.parse("2026-08-24T00:00:00.000Z"),
      pendingAssignment: "verify #41",
      locator: {
        tmuxSession: "legion-omp",
        tmuxWindowId: "@42",
        tmuxPaneId: "%7",
        socketPath: "/state/workers/tester.sock",
      },
    };
    state.workerAdmission.queue.push(token);

    // Seed the cached client the decision phase reads from `workerClients` (established via a
    // prior connect, exactly as a real idle-resume queue entry would have one already cached).
    await processes.reconnectWorkers();
    expect(client.prompts).toEqual([]);

    await processes.reconcileWorkerAdmission();

    // The failed prompt never stranded the assignment: the token is still queued (never shifted
    // off for this decision) and its pendingAssignment is untouched (promptExistingWorker only
    // clears it after client.prompt() succeeds).
    expect(managedState.workerAdmission.queue).toEqual([token]);
    const claim = managedState.roles[token];
    if (!claim || !("issue" in claim)) throw new Error("tester claim disappeared");
    expect(claim.pendingAssignment).toBe("verify #41");
    expect(claim.locator).toBeDefined();
    expect(client.prompts).toEqual([]);
  });

  it("clears a dead worker's locator and promotes the queue when its socket closes and one reconnect attempt fails", async () => {
    const clients: Array<{ close(): void }> = [];
    let connectAttempts = 0;
    const publications: Array<{ subject: string; json: string }> = [];
    let resolvePromoted: (() => void) | undefined;
    const promoted = new Promise<void>((resolve) => {
      resolvePromoted = resolve;
    });
    const plannerToken = roleToken("omp", root, "planner");
    const testerToken = roleToken("omp", root, "tester");
    const architectTopic = roleTopic(roleToken("omp", root, "architect"));
    const { processes, managedState, stateDir } = await workerCapFixture(1, {
      sleep: async () => {},
      connectWorkerRpc: async () => {
        connectAttempts += 1;
        // The very first connection (workerReady, below) succeeds; the reconnect attempt
        // `onWorkerClientClosed` makes after the socket closes fails, confirming the worker dead.
        if (connectAttempts > 1) throw new Error("ECONNREFUSED");
        const client = fakeWorkerRpcClient();
        clients.push(client);
        return client;
      },
      natsPublish: (subject, json) => {
        publications.push({ subject, json });
        if (subject === architectTopic && json.includes("worker-started")) resolvePromoted?.();
      },
    });

    await processes.spawnWorker(root, root, "planner", "plan #41");
    await processes.spawnWorker(root, root, "tester", "verify #41");
    expect(managedState.workerAdmission.queue).toEqual([testerToken]);

    const plannerClaim = managedState.roles[plannerToken];
    if (!plannerClaim || !("issue" in plannerClaim)) throw new Error("planner claim missing");
    plannerClaim.sessionId = "ses_planner";
    // Simulates /worker/started already having registered this generation's real OMP session
    // file, so markWorkerDead's resumeSessionFile-preservation has something real to preserve.
    if (plannerClaim.locator) {
      plannerClaim.locator.ompSessionFile = path.join(stateDir, "planner-session.json");
    }
    await processes.workerReady(root, "planner", "ses_planner", plannerClaim.generation ?? 1);
    expect(clients).toHaveLength(1);

    clients[0]?.close();
    await promoted;

    // At least 1 (workerReady) + 1 (onWorkerClientClosed's own reconnect attempt) + 1
    // (retireWorkerLocator's own attempt to shut the dead shim down) — all after the first
    // fail. Not an exact count: either planner's or the newly-promoted tester's own boot
    // watchdog may also probe the shim socket for liveness before this point (see
    // `armBootWatchdog`/`probeWorkerAlive`), adding its own connect attempt.
    expect(connectAttempts).toBeGreaterThanOrEqual(3);
    expect(managedState.workerAdmission.queue).toEqual([]);
    const deadPlannerClaim = managedState.roles[plannerToken];
    if (!deadPlannerClaim || !("issue" in deadPlannerClaim)) {
      throw new Error("planner claim disappeared");
    }
    // Locator cleared (not left dangling forever), its ompSessionFile preserved as
    // resumeSessionFile so a later respawn/promotion still resumes the same agent.
    expect(deadPlannerClaim.locator).toBeUndefined();
    expect(deadPlannerClaim.resumeSessionFile).toBe(path.join(stateDir, "planner-session.json"));
    const promotedClaim = managedState.roles[testerToken];
    if (!promotedClaim || !("issue" in promotedClaim)) throw new Error("tester claim missing");
    expect(promotedClaim.locator).toBeDefined();
    expect(publications).toContainEqual({
      subject: architectTopic,
      json: JSON.stringify({ type: "worker-started", issue: root, role: "tester" }),
    });
  });

  it("treats a reconnected socket whose get_state fails as busy, not dead, keeping its locator", async () => {
    const state = newLegionState("omp", 1);
    const token = roleToken("omp", root, "tester");
    state.roles[token] = {
      issue: root,
      role: "tester",
      sessionId: "ses_tester",
      generation: 1,
      pendingAssignment: "verify #41",
      locator: {
        tmuxSession: "legion-omp",
        tmuxWindowId: "@42",
        tmuxPaneId: "%7",
        socketPath: "/state/workers/tester.sock",
      },
    };
    const client = fakeWorkerRpcClient();
    client.getStateImpl = async () => {
      throw new Error("get_state timed out after 5000ms");
    };
    const commands: string[][] = [];
    const { manager: processes, state: managedState } = manager(state, {
      connectWorkerRpc: async () => client,
      run: async (command) => {
        commands.push(command);
        return { stdout: "", exitCode: 0 };
      },
    });

    await processes.reconnectWorkers();

    expect(client.getStateCalls).toBe(1);
    // The connect itself succeeded; only the follow-up get_state request timed out. Never a
    // reason to kill a live worker — the claim (and its locator) is left exactly as is.
    const claim = managedState.roles[token];
    if (!claim || !("issue" in claim)) throw new Error("worker claim disappeared");
    expect(claim.locator).toBeDefined();
    expect(claim.pendingAssignment).toBe("verify #41");
    expect(
      commands.some(
        (command) =>
          command[0] === "tmux" && (command[1] === "kill-pane" || command[1] === "kill-window")
      )
    ).toBeFalse();
  });

  it("mints nothing for a queued worker when an idle reconnect fires onIdle before enableWorkerPromotion, then promotes it once enabled", async () => {
    const idleToken = roleToken("omp", root, "planner");
    const queuedToken = roleToken("omp", root, "tester");
    let mintCalls = 0;
    const { processes, state, managedState } = await workerCapFixture(
      1,
      {
        connectWorkerRpc: async () => {
          const client = fakeWorkerRpcClient();
          // Simulates the real worker-rpc client's get_state response seeding runState from
          // isStreaming: false — synchronously fires the registered onIdle callback (exactly
          // like a real boot-time reconnect probe would) while still inside reconnectWorkers,
          // before `api` (and this gate) would ever be assigned in the real boot sequence.
          client.getStateImpl = async () => {
            client.emitRunState("idle");
            return { isStreaming: false };
          };
          return client;
        },
        mintWorkerBootToken: async () => {
          mintCalls += 1;
          return "worker-boot-token";
        },
      },
      { skipEnablePromotion: true }
    );
    state.roles[idleToken] = {
      issue: root,
      role: "planner",
      generation: 1,
      sessionId: "ses_planner",
      locator: {
        tmuxSession: "legion-omp",
        tmuxWindowId: "@42",
        tmuxPaneId: "%1",
        socketPath: "/state/workers/planner.sock",
      },
    };
    state.roles[queuedToken] = {
      issue: root,
      role: "tester",
      pendingAssignment: "verify #41",
    };
    state.workerAdmission.queue.push(queuedToken);

    await processes.reconnectWorkers();

    // The onIdle trigger fired mid-reconnect but the gate was still closed: no boot token was
    // ever minted (the real bug this fixes — `mintWorkerBootToken` reads `api` by reference,
    // which does not exist yet at this exact point in the real boot sequence), and the queued
    // assignment is untouched.
    expect(mintCalls).toBe(0);
    expect(managedState.workerAdmission.queue).toEqual([queuedToken]);

    processes.enableWorkerPromotion();
    await processes.reconcileWorkerAdmission();

    // Now that the gate is open, the exact same queued assignment promotes normally.
    expect(mintCalls).toBe(1);
    expect(managedState.workerAdmission.queue).toEqual([]);
    const promotedClaim = managedState.roles[queuedToken];
    if (!promotedClaim || !("issue" in promotedClaim)) throw new Error("tester claim missing");
    expect(promotedClaim.locator).toBeDefined();
  });

  it("goes straight to markWorkerDead on a second close for the same generation, without chaining into a second reconnect", async () => {
    const token = roleToken("omp", root, "tester");
    const clients: ReturnType<typeof fakeWorkerRpcClient>[] = [];
    let connectCalls = 0;
    const { processes, state, managedState } = await workerCapFixture(1, {
      sleep: async () => {},
      connectWorkerRpc: async () => {
        connectCalls += 1;
        const client = fakeWorkerRpcClient();
        clients.push(client);
        return client;
      },
    });
    state.roles[token] = {
      issue: root,
      role: "tester",
      generation: 1,
      sessionId: "ses_tester",
      pendingAssignment: "verify #41",
      locator: {
        tmuxSession: "legion-omp",
        tmuxWindowId: "@42",
        tmuxPaneId: "%7",
        socketPath: "/state/workers/tester.sock",
      },
    };

    // Establishes the initial cached connection (connect #1).
    await processes.workerReady(root, "tester", "ses_tester", 1);
    expect(connectCalls).toBe(1);
    const firstClient = clients[0];
    if (!firstClient) throw new Error("first client never connected");

    // First close: onWorkerClientClosed's one-reconnect-attempt succeeds (connect #2) — the
    // worker is reconnected, not yet dead, and this generation's one reconnect credit is spent.
    firstClient.close();
    await Bun.sleep(0);
    await Bun.sleep(0);
    expect(connectCalls).toBe(2);
    expect(managedState.roles[token]).toMatchObject({ locator: { tmuxPaneId: "%7" } });
    const secondClient = clients[1];
    if (!secondClient) throw new Error("reconnect never created a second client");

    // Second close, same generation: goes straight to markWorkerDead instead of attempting yet
    // another reconnect — connect #3 below is only retireWorkerLocator's own best-effort
    // shutdown probe (an unconditional part of confirming death), never a second reconnect
    // attempt. If the one-reconnect-per-generation guard were broken, this would instead chain
    // into a fresh `workerClient` reconnect call before even reaching retirement, landing at
    // connect #4 (or more, looping) instead of settling at exactly 3.
    secondClient.close();
    await Bun.sleep(0);
    await Bun.sleep(0);
    await Bun.sleep(0);

    expect(connectCalls).toBe(3);
    const claim = managedState.roles[token];
    if (!claim || !("issue" in claim)) throw new Error("worker claim disappeared");
    expect(claim.locator).toBeUndefined();
  });

  it("never deletes a newer launch's locator when a stale dead-worker confirmation catches up late", async () => {
    const state = newLegionState("omp", 1);
    const token = roleToken("omp", root, "tester");
    const staleLocator = {
      tmuxSession: "legion-omp",
      tmuxWindowId: "@42",
      tmuxPaneId: "%7",
      socketPath: "/state/workers/tester.sock",
    };
    state.roles[token] = {
      issue: root,
      role: "tester",
      generation: 1,
      pendingAssignment: "verify #41",
      locator: staleLocator,
    };
    const connectGate = Promise.withResolvers<void>();
    const commands: string[][] = [];
    const { manager: processes, state: managedState } = manager(state, {
      // A reconnect attempt for the STALE locator's socket that hangs until the test releases
      // it, then fails — confirming (stale) death only after a newer launch has already
      // replaced this claim's locator.
      connectWorkerRpc: async () => {
        await connectGate.promise;
        throw new Error("ECONNREFUSED");
      },
      run: async (command) => {
        commands.push(command);
        return { stdout: "", exitCode: 0 };
      },
    });

    const reconnectPromise = processes.reconnectWorkers();

    // While the stale reconnect attempt is still in flight, a fresh launch replaces this
    // token's claim with a genuinely newer locator (a different pane).
    const freshLocator = {
      tmuxSession: "legion-omp",
      tmuxWindowId: "@42",
      tmuxPaneId: "%99",
      socketPath: "/state/workers/tester.sock",
    };
    state.roles[token] = {
      issue: root,
      role: "tester",
      generation: 2,
      pendingAssignment: "verify #55",
      locator: freshLocator,
    };

    connectGate.resolve();
    await reconnectPromise;

    // The stale confirmation must never delete the newer launch's locator, nor kill its pane.
    const claim = managedState.roles[token];
    if (!claim || !("issue" in claim)) throw new Error("worker claim disappeared");
    expect(claim.locator).toEqual(freshLocator);
    expect(claim.generation).toBe(2);
    expect(
      commands.some(
        (command) =>
          command[0] === "tmux" && (command[1] === "kill-pane" || command[1] === "kill-window")
      )
    ).toBeFalse();
  });

  it("retires an unlaunchable queue head at MAX_LAUNCH_FAILURES instead of blocking the queue forever", async () => {
    const failingToken = roleToken("omp", root, "planner");
    const okToken = roleToken("omp", root, "tester");
    const records: Array<{ subject: string; json: string }> = [];
    let resolvePromoted: (() => void) | undefined;
    const promoted = new Promise<void>((resolve) => {
      resolvePromoted = resolve;
    });
    const architectTopic = roleTopic(roleToken("omp", root, "architect"));
    const { processes, state, managedState, stateDir } = await workerCapFixture(2, {
      natsPublish: (subject, json) => {
        records.push({ subject, json });
        if (subject === architectTopic && json.includes("worker-started")) resolvePromoted?.();
      },
    });
    state.roles[failingToken] = {
      issue: root,
      role: "planner",
      pendingAssignment: "plan #41",
      // A deterministic, permanent failure (missing session file, never appears on retry) — the
      // same mechanism "fails a worker respawn loudly..." above exercises directly.
      resumeSessionFile: path.join(stateDir, "missing-planner-session.json"),
      launchFailures: 2,
    };
    state.roles[okToken] = {
      issue: root,
      role: "tester",
      pendingAssignment: "verify #41",
    };
    state.workerAdmission.queue.push(failingToken, okToken);

    await processes.reconcileWorkerAdmission();
    await promoted;

    // The permanently-failing head never blocked tester behind it: both were drained in the
    // same pass, one retired at the failure threshold, the other launched.
    expect(managedState.workerAdmission.queue).toEqual([]);
    const failingClaim = managedState.roles[failingToken];
    if (!failingClaim || !("issue" in failingClaim)) throw new Error("planner claim missing");
    expect(failingClaim.launchFailures).toBe(3);
    expect(failingClaim.locator).toBeUndefined();
    const okClaim = managedState.roles[okToken];
    if (!okClaim || !("issue" in okClaim)) throw new Error("tester claim missing");
    expect(okClaim.locator).toBeDefined();
    expect(records).toContainEqual({
      subject: architectTopic,
      json: JSON.stringify({ type: "launch-failed", issue: root, role: "planner", failures: 3 }),
    });
    // launch-failed publishes exactly once, at the exact tick the threshold is crossed — not
    // again on some later, otherwise-nonexistent retry of the now-retired token.
    expect(records.filter((record) => record.json.includes("launch-failed"))).toHaveLength(1);
  });

  it("rotates a below-threshold launch failure to the tail instead of blocking the queue behind it", async () => {
    const failingToken = roleToken("omp", root, "planner");
    const okToken = roleToken("omp", root, "tester");
    const { processes, state, managedState, stateDir } = await workerCapFixture(1);
    state.roles[failingToken] = {
      issue: root,
      role: "planner",
      pendingAssignment: "plan #41",
      // A deterministic, permanent failure (missing session file, never appears on retry) — but
      // launchFailures starts at 0, so one attempt stays *below* MAX_LAUNCH_FAILURES (3).
      resumeSessionFile: path.join(stateDir, "missing-planner-session.json"),
    };
    state.roles[okToken] = {
      issue: root,
      role: "tester",
      pendingAssignment: "verify #41",
    };
    state.workerAdmission.queue.push(failingToken, okToken);

    // Cap 1: once okToken successfully launches (occupying the only slot), the drain loop's own
    // cap check stops it from immediately re-attempting the rotated failingToken again in the
    // same pass — isolating this test to exactly one below-threshold rotation.
    await processes.reconcileWorkerAdmission();

    // failingToken failed once (below threshold), was rotated to the tail rather than left
    // blocking the head, and okToken — now at the head — launched successfully in the same pass.
    expect(managedState.workerAdmission.queue).toEqual([failingToken]);
    const failingClaim = managedState.roles[failingToken];
    if (!failingClaim || !("issue" in failingClaim)) throw new Error("planner claim missing");
    expect(failingClaim.launchFailures).toBe(1);
    expect(failingClaim.locator).toBeUndefined();
    const okClaim = managedState.roles[okToken];
    if (!okClaim || !("issue" in okClaim)) throw new Error("tester claim missing");
    expect(okClaim.locator).toBeDefined();
  });

  it("stops the drain after one below-threshold failure on a single-item queue instead of burning every MAX_LAUNCH_FAILURES attempt in one pass", async () => {
    const failingToken = roleToken("omp", root, "planner");
    let launchAttempts = 0;
    const { processes, state, managedState, stateDir } = await workerCapFixture(1, {
      run: async (command) => {
        if (command[0] === "jj" && command[1] === "workspace" && command[2] === "add") {
          launchAttempts += 1;
        }
        return { stdout: "", exitCode: 0 };
      },
    });
    state.roles[failingToken] = {
      issue: root,
      role: "planner",
      pendingAssignment: "plan #41",
      // A deterministic, permanent failure (missing session file, never appears on retry) — but
      // launchFailures starts at 0, so one attempt stays *below* MAX_LAUNCH_FAILURES (3). This
      // queue holds ONLY this one token — a rotate-in-place (shift then push, same array)
      // re-exposes the exact same head, which is the scenario the fix targets.
      resumeSessionFile: path.join(stateDir, "missing-planner-session.json"),
    };
    state.workerAdmission.queue.push(failingToken);

    await processes.reconcileWorkerAdmission();

    // A single below-threshold rotation on a one-item queue exposes the SAME head again — the
    // drain must stop there instead of looping straight back into it, which would otherwise
    // burn all 3 MAX_LAUNCH_FAILURES attempts in this one synchronous pass instead of leaving
    // the retry to the periodic sweep or the next idle/dead event.
    expect(launchAttempts).toBe(1);
    expect(managedState.workerAdmission.queue).toEqual([failingToken]);
    const failingClaim = managedState.roles[failingToken];
    if (!failingClaim || !("issue" in failingClaim)) throw new Error("planner claim missing");
    expect(failingClaim.launchFailures).toBe(1);
    expect(failingClaim.locator).toBeUndefined();
  });

  it("counts a sub-architect claim toward the cap even after its issue graduates into its own tree root", async () => {
    const stateDir = await temporaryDir();
    const state = newLegionState("omp", 2);
    tree(state);
    state.issues[child] = {
      key: child,
      title: "Child",
      status: "in_progress",
      parent: root,
      children: [],
    };
    // The child issue has since graduated into its own tree root: rootForIssue(child) now
    // resolves to `child` itself, not `root` — exactly the case the old root-architect exclusion
    // clause in runningWorkerCount misfired on, undercounting this claim and over-admitting.
    state.trees[child] = {
      root: child,
      generation: 1,
      status: "active",
      launchFailures: 0,
    };
    const subArchitectToken = roleToken("omp", child, "architect");
    state.roles[subArchitectToken] = {
      issue: child,
      role: "architect",
      locator: {
        tmuxSession: "legion-omp",
        tmuxWindowId: "@99",
        tmuxPaneId: "%99",
        socketPath: "/state/workers/sub-architect.sock",
      },
    };

    const { manager: processes, state: managedState } = manager(state, {
      config: config(stateDir, { workerCap: 1 }),
    });

    const result = await processes.spawnWorker(root, root, "planner", "plan #41");

    expect(result.status).toBe("queued");
    expect(managedState.workerAdmission.queue).toEqual([roleToken("omp", root, "planner")]);
  });

  it("does not let a slow launch block a concurrent different-role admission decision", async () => {
    const stateDir = await temporaryDir();
    const state = newLegionState("omp", 1);
    tree(state);
    // A different issue from the tree root, so the two spawns below never contend on the
    // per-issue `issueLaunchQueue` window-creation serialization — only on `admissionLock`.
    state.issues[child] = {
      key: child,
      title: "Child",
      status: "in_progress",
      parent: root,
      children: [],
    };
    const plannerGate = Promise.withResolvers<void>();
    const { manager: processes } = manager(state, {
      config: config(stateDir, { workerCap: 2 }),
      run: async (command) => {
        if (
          command[0] === "tmux" &&
          command[1] === "split-window" &&
          command.includes("LEGION_ROLE=planner")
        ) {
          // Held open for the whole test: proves the admission decision for a different role
          // does not wait on this launch, since the lock scope no longer wraps launchWorker.
          await plannerGate.promise;
          return { stdout: "%201 12345\n", exitCode: 0 };
        }
        if (command[0] === "tmux" && command[1] === "new-window") {
          return { stdout: "@43 %202 23456\n", exitCode: 0 };
        }
        return { stdout: "", exitCode: 0 };
      },
    });

    const plannerPromise = processes.spawnWorker(root, root, "planner", "plan #41");
    const testerPromise = processes.spawnWorker(root, child, "tester", "verify #43");

    const testerResult = await testerPromise;
    expect(testerResult.status).toBe("spawned");

    plannerGate.resolve();
    const plannerResult = await plannerPromise;
    expect(plannerResult.status).toBe("spawned");
  });

  it("a concurrent admission decision during the post-locator-write, pre-saveState window sees exactly one occupied slot, not two, and only releases/re-checks the queue after the save settles", async () => {
    const saveStateCalled = Promise.withResolvers<void>();
    const releaseSave = Promise.withResolvers<void>();
    let saveStateCalls = 0;
    const sequence: string[] = [];
    const plannerToken = roleToken("omp", root, "planner");
    const { processes, managedState } = await workerCapFixture(2, {
      saveState: async () => {
        saveStateCalls += 1;
        // Only planner's own post-locator-write save is held open. `launchWorker` writes the
        // claim's locator (in memory) *before* calling this, and the caller's `finally` — which
        // releases planner's `launching` reservation and re-checks the queue — only runs once
        // this whole `launchWorker` call (including this save) resolves. During this exact
        // window planner's reservation is still held in `this.launching` AND its locator is
        // already written — `runningWorkerCount()` must count that as one occupied slot, not
        // two.
        if (saveStateCalls === 1) {
          saveStateCalled.resolve();
          await releaseSave.promise;
          sequence.push("saveState resolved");
        }
      },
      // Test-only hook (see `WorkerAdmissionDeps.onAdmissionEvent`'s doc comment): records the
      // exact moment `launchOrQueue`'s own `finally` releases planner's reservation and
      // triggers the queue re-check, so this test can assert both happen strictly after the
      // gated save resolves — never during, and never because of the concurrent tester
      // decision racing in below.
      onAdmissionEvent: (token, event) => {
        if (token !== plannerToken) return;
        sequence.push(event === "reservation-released" ? "release" : "promote");
      },
    });

    const plannerPromise = processes.spawnWorker(root, root, "planner", "plan #41");
    await saveStateCalled.promise;

    // Racing in right as planner's locator write has landed but before its saveState — and
    // therefore the release of its own `launching` reservation — has settled: at cap 2 this
    // must LAUNCH, not queue. A formula that sums `this.launching` unconditionally alongside
    // every locator-bearing claim would double-count planner here (reserved *and* located) and
    // wrongly report both of cap 2's slots already occupied, queuing this tester instead.
    const testerResult = await processes.spawnWorker(root, root, "tester", "verify #41");
    expect(testerResult.status).toBe("spawned");

    // Still gated: planner's own release/promote must not have fired yet, regardless of the
    // concurrent tester decision above (that decision never touches planner's reservation).
    expect(sequence).toEqual([]);

    releaseSave.resolve();
    const plannerResult = await plannerPromise;
    expect(plannerResult.status).toBe("spawned");
    expect(managedState.workerAdmission.queue).toEqual([]);
    // Release and the queue-drain trigger both fire exactly once, strictly after the gated
    // save resolves — never before it, and never more than once.
    expect(sequence).toEqual(["saveState resolved", "release", "promote"]);
  });

  it("retires the pane instead of writing a claim when closeTree closes the tree while a launch is still in flight", async () => {
    const stateDir = await temporaryDir();
    const state = newLegionState("omp", 1);
    tree(state);
    const launchGate = Promise.withResolvers<void>();
    const commands: string[][] = [];
    const { manager: processes, state: managedState } = manager(state, {
      config: config(stateDir, { workerCap: 1 }),
      run: async (command) => {
        commands.push(command);
        // The tester splits into the root's own already-alive window (from `tree()`), rather
        // than opening a fresh one.
        if (command[0] === "tmux" && command[1] === "split-window") {
          // Held open until closeTree below has actually started -- proves launchWorker
          // re-checks after this I/O, not before it.
          await launchGate.promise;
          return { stdout: "%1 12345\n", exitCode: 0 };
        }
        return { stdout: "", exitCode: 0 };
      },
    });

    const spawnPromise = processes.spawnWorker(root, root, "tester", "verify #41");
    // Poll until the launch has actually reached its blocked `split-window` call before racing:
    // `inFlightLaunches` makes `closeTree` await this exact decision before concluding the
    // tree is empty, so starting the race any earlier would just serialize the two calls.
    for (
      let attempt = 0;
      attempt < 100 && !commands.some((c) => c[1] === "split-window");
      attempt++
    ) {
      await new Promise((resolve) => setTimeout(resolve, 1));
    }
    expect(commands.some((c) => c[1] === "split-window")).toBe(true);

    const closePromise = processes.closeTree(root);
    launchGate.resolve();
    const spawnResult = await spawnPromise.catch((caught: unknown) => caught);
    await closePromise;

    // The just-opened pane is retired and reported as a closing error, not a silent success or
    // a launch failure -- `spawnWorker`/`launchWorker` never write a claim for a tree that has
    // already started tearing down.
    expect(spawnResult).toBeInstanceOf(TreeClosingError);
    expect(managedState.trees[root]?.status).toBe("closed");
    // No zombie claim was ever written for a tree that closed mid-launch, and the pane this
    // launch just opened was retired (killed), not left running unrecorded and forever
    // occupying a running-worker slot.
    expect(managedState.roles[roleToken("omp", root, "tester")]).toBeUndefined();
    expect(
      commands.some(
        (command) =>
          command[0] === "tmux" && (command[1] === "kill-pane" || command[1] === "kill-window")
      )
    ).toBeTrue();
  });

  it("keeps closeTree from finishing while a QUEUE-PROMOTED launch is still in flight, not only a direct spawnWorker one", async () => {
    // Without the constructor's `trackLaunch` wrap on the `launchWorker` dependency
    // `WorkerAdmission`'s own queue-drain calls directly (`promoteQueuedWorker`, bypassing
    // `spawnWorker`'s own `trackLaunch` entirely), closeTree's fixed-point loop would see an
    // empty `state.roles` snapshot (this promoted launch has not written its claim yet), finish,
    // and clear `closingTrees` -- all while this exact promotion is still blocked mid-flight.
    const stateDir = await temporaryDir();
    const state = newLegionState("omp", 1);
    tree(state);
    const testerToken = roleToken("omp", root, "tester");
    state.roles[testerToken] = {
      issue: root,
      role: "tester",
      pendingAssignment: "verify #41",
    };
    state.workerAdmission.queue.push(testerToken);

    const launchGate = Promise.withResolvers<void>();
    const commands: string[][] = [];
    const { manager: processes, state: managedState } = manager(state, {
      config: config(stateDir, { workerCap: 1 }),
      run: async (command) => {
        commands.push(command);
        // The tester splits into the root's own already-alive window (from `tree()`) — held
        // open until closeTree below has had every real chance to finish, proving the PROMOTED
        // launch (not just a direct spawnWorker call) is what `inFlightLaunches` awaits.
        if (command[0] === "tmux" && command[1] === "split-window") {
          await launchGate.promise;
          return { stdout: "%1 12345\n", exitCode: 0 };
        }
        return { stdout: "", exitCode: 0 };
      },
    });

    // Nothing else occupies the cap, so this promotes immediately through WorkerAdmission's own
    // queue-drain, never through `spawnWorker`.
    const drainPromise = processes.reconcileWorkerAdmission();

    for (
      let attempt = 0;
      attempt < 100 && !commands.some((c) => c[1] === "split-window");
      attempt++
    ) {
      await new Promise((resolve) => setTimeout(resolve, 1));
    }
    expect(commands.some((c) => c[1] === "split-window")).toBe(true);

    let closeSettled = false;
    const closePromise = processes.closeTree(root).then(() => {
      closeSettled = true;
    });

    // Give closeTree every real chance to run its (otherwise-empty) worker loop and finish while
    // the promoted launch above is still blocked on `split-window`.
    await new Promise((resolve) => setTimeout(resolve, 20));
    expect(closeSettled).toBe(false);

    launchGate.resolve();
    await drainPromise;
    await closePromise;

    expect(closeSettled).toBe(true);
    expect(managedState.trees[root]?.status).toBe("closed");
    // No zombie claim was ever left for a tree that closed mid-promotion, and the promoted
    // launch's own queue entry does not survive a close it never got the chance to run inside.
    expect(managedState.roles[testerToken]).toBeUndefined();
    expect(managedState.workerAdmission.queue).toEqual([]);
    // Exactly the one pane this launch opened -- no second pane from a retry treating the
    // TreeClosingError like an ordinary launch failure and rotating/relaunching this token.
    expect(
      commands.filter((command) => command[0] === "tmux" && command[1] === "split-window")
    ).toHaveLength(1);
  });

  it("re-evaluates a persisted running-worker queue against a raised cap across a restart", async () => {
    const testerToken = roleToken("omp", root, "tester");
    const records: Array<{ subject: string; json: string }> = [];
    let resolvePromoted: (() => void) | undefined;
    const promoted = new Promise<void>((resolve) => {
      resolvePromoted = resolve;
    });
    const architectTopic = roleTopic(roleToken("omp", root, "architect"));
    const { processes, state, managedState } = await workerCapFixture(2, {
      natsPublish: (subject, json) => {
        records.push({ subject, json });
        if (subject === architectTopic && json.includes("worker-started")) resolvePromoted?.();
      },
    });
    state.roles[testerToken] = {
      issue: root,
      role: "tester",
      pendingAssignment: "verify #41",
    };
    state.workerAdmission.queue.push(testerToken);

    await processes.reconcileWorkerAdmission();
    await promoted;

    expect(managedState.workerAdmission.queue).toEqual([]);
    const promotedClaim = managedState.roles[testerToken];
    if (!promotedClaim || !("issue" in promotedClaim)) throw new Error("tester claim missing");
    expect(promotedClaim.locator).toBeDefined();
    expect(records).toContainEqual({
      subject: architectTopic,
      json: JSON.stringify({ type: "worker-started", issue: root, role: "tester" }),
    });
  });

  it("admits exactly one of two concurrent different-role spawns racing for the last slot", async () => {
    const stateDir = await temporaryDir();
    const workspace = path.join(stateDir, "workspaces", "sjawhar", "legion", "issue-42");
    await mkdir(workspace, { recursive: true });
    const state = newLegionState("omp", 1);
    state.trees[root] = {
      root,
      generation: 1,
      status: "active",
      launchFailures: 0,
    };
    const {
      manager: processes,
      state: managedState,
      commands,
    } = manager(state, {
      config: config(stateDir, { workerCap: 1 }),
    });

    const [plannerResult, testerResult] = await Promise.all([
      processes.spawnWorker(root, root, "planner", "plan #41"),
      processes.spawnWorker(root, root, "tester", "verify #41"),
    ]);

    expect([plannerResult.status, testerResult.status].sort()).toEqual(["queued", "spawned"]);
    expect(
      commands.filter((command) => command[0] === "tmux" && command[1] === "new-window")
    ).toHaveLength(1);
    const plannerClaim = managedState.roles[roleToken("omp", root, "planner")];
    const testerClaim = managedState.roles[roleToken("omp", root, "tester")];
    if (!plannerClaim || !("issue" in plannerClaim) || !testerClaim || !("issue" in testerClaim)) {
      throw new Error("both worker claims must be recorded");
    }
    const spawnedClaim = plannerResult.status === "spawned" ? plannerClaim : testerClaim;
    const queuedClaim = plannerResult.status === "spawned" ? testerClaim : plannerClaim;
    expect(spawnedClaim.locator).toBeDefined();
    expect(queuedClaim.locator).toBeUndefined();
    expect(managedState.workerAdmission.queue).toEqual([
      plannerResult.status === "queued"
        ? roleToken("omp", root, "planner")
        : roleToken("omp", root, "tester"),
    ]);
  });

  it("promotes exactly one queued worker when two running workers go idle in the same tick", async () => {
    const stateDir = await temporaryDir();
    const workspace = path.join(stateDir, "workspaces", "sjawhar", "legion", "issue-42");
    await mkdir(workspace, { recursive: true });
    const state = newLegionState("omp", 1);
    state.trees[root] = {
      root,
      generation: 1,
      status: "active",
      launchFailures: 0,
    };
    const clients: Array<{ emitRunState(state: "running" | "idle"): void }> = [];
    const publications: Array<{ subject: string; json: string }> = [];
    let resolvePromoted: (() => void) | undefined;
    const promoted = new Promise<void>((resolve) => {
      resolvePromoted = resolve;
    });
    const architectTopic = roleTopic(roleToken("omp", root, "architect"));
    const {
      manager: processes,
      state: managedState,
      commands,
    } = manager(state, {
      config: config(stateDir, { workerCap: 2 }),
      connectWorkerRpc: async () => {
        const client = fakeWorkerRpcClient();
        clients.push(client);
        return client;
      },
      natsPublish: (subject, json) => {
        publications.push({ subject, json });
        if (subject === architectTopic && json.includes("worker-started")) resolvePromoted?.();
      },
    });

    await processes.spawnWorker(root, root, "planner", "plan #41");
    await processes.spawnWorker(root, root, "implementer", "implement #41");
    const testerResult = await processes.spawnWorker(root, root, "tester", "verify #41");
    expect(testerResult).toEqual({ status: "queued", roleToken: roleToken("omp", root, "tester") });

    for (const role of ["planner", "implementer"] as const) {
      const token = roleToken("omp", root, role);
      const claim = managedState.roles[token];
      if (!claim || !("issue" in claim)) throw new Error(`${role} claim missing`);
      claim.sessionId = `ses_${role}`;
      await processes.workerReady(root, role, `ses_${role}`, claim.generation ?? 1);
    }
    expect(clients).toHaveLength(2);

    // Both running workers finish their turn in the same synchronous tick, racing to promote the
    // single queued tester. The admission lock's inside-the-critical-section re-peek of the queue
    // head must let only the first through; the second must see the head already moved on.
    clients[0]?.emitRunState("idle");
    clients[1]?.emitRunState("idle");
    await promoted;

    expect(managedState.workerAdmission.queue).toEqual([]);
    expect(
      commands.filter((command) => command[0] === "tmux" && command[1] === "new-window")
    ).toHaveLength(1);
    expect(
      commands.filter((command) => command[0] === "tmux" && command[1] === "split-window")
    ).toHaveLength(2);
    const testerClaim = managedState.roles[roleToken("omp", root, "tester")];
    if (!testerClaim || !("issue" in testerClaim)) throw new Error("tester claim missing");
    expect(testerClaim.locator).toBeDefined();
    expect(
      publications.filter(
        (publication) =>
          publication.subject === architectTopic && publication.json.includes("worker-started")
      )
    ).toHaveLength(1);
  });

  it("refuses to enqueue a spawn against an already-closed tree, even at cap, instead of queueing a locator-less claim nothing would ever prune", async () => {
    const stateDir = await temporaryDir();
    const state = newLegionState("omp", 1);
    tree(state);
    const treeState = state.trees[root];
    if (!treeState) throw new Error("tree missing");
    // Already fully closed -- not merely closing (`closingTrees` is empty here, unlike the
    // mid-teardown test below): the entry check must catch this via the SAME `isTreeGone`
    // predicate `launchWorker` itself uses, not just `closingTrees.has`.
    treeState.status = "closed";
    const { manager: processes, state: managedState } = manager(state, {
      config: config(stateDir, { workerCap: 0 }),
    });

    const error = await processes
      .spawnWorker(root, root, "tester", "verify #41")
      .catch((caught: unknown) => caught);

    expect(error).toBeInstanceOf(TreeClosingError);
    // Never enqueued: before this fix, a spawn against an already-closed tree at cap would still
    // reach `launchOrQueue` and push a locator-less claim that nothing left running would ever
    // prune (`pruneQueueForTree` only runs from inside an active `closeTreeLocked` call).
    expect(managedState.workerAdmission.queue).toEqual([]);
    expect(managedState.roles[roleToken("omp", root, "tester")]).toBeUndefined();
  });

  it("drops a queued token whose tree is already closed instead of leaving it wedged at the head forever, then promotes the next queued token", async () => {
    const stateDir = await temporaryDir();
    const state = newLegionState("omp", 1);
    tree(state);
    const treeState = state.trees[root];
    if (!treeState) throw new Error("tree missing");
    treeState.status = "closed";
    // Models a token that was validly queued before its tree closed (or a legacy pre-fix wedge):
    // locator-less, still carrying a pending assignment, sitting at the head of the FIFO queue.
    const deadToken = roleToken("omp", root, "tester");
    state.roles[deadToken] = { issue: root, role: "tester", pendingAssignment: "verify #41" };

    const otherRoot = "LEGION-99";
    state.trees[otherRoot] = {
      root: otherRoot,
      generation: 1,
      status: "active",
      launchFailures: 0,
    };
    const liveToken = roleToken("omp", otherRoot, "planner");
    state.roles[liveToken] = { issue: otherRoot, role: "planner", pendingAssignment: "plan #99" };
    state.workerAdmission.queue.push(deadToken, liveToken);

    const { manager: processes, state: managedState } = manager(state, {
      config: config(stateDir, { workerCap: 1 }),
    });

    await processes.reconcileWorkerAdmission();

    // The dead entry never blocks the live one behind it: dropped with no failure accounting
    // (not rotated to the tail, not left wedged at the head for every future drain to retry and
    // fail identically), and the queue is now empty because the live token was promoted in the
    // same pass.
    expect(managedState.workerAdmission.queue).toEqual([]);
    expect(managedState.roles[deadToken]).toBeUndefined();
    const liveClaim = managedState.roles[liveToken];
    if (!liveClaim || !("issue" in liveClaim)) throw new Error("live claim missing");
    expect(liveClaim.locator).toBeDefined();
  });

  it("prunes every spawn capability recorded for a tree once it closes, so a pre-shutdown spawn token can no longer authorize the legacy role-backing route into recreating a claim", async () => {
    const stateDir = await temporaryDir();
    const state = newLegionState("omp", 1);
    tree(state);
    const closingSpawnToken = "spawn-token-for-closing-tree";
    state.spawnCapabilities[spawnCapabilityKey(closingSpawnToken)] = {
      tree: root,
      issue: root,
      role: "tester",
    };
    const otherRoot = "LEGION-77";
    state.trees[otherRoot] = {
      root: otherRoot,
      generation: 1,
      status: "active",
      launchFailures: 0,
    };
    const unrelatedSpawnToken = "spawn-token-for-a-different-tree";
    state.spawnCapabilities[spawnCapabilityKey(unrelatedSpawnToken)] = {
      tree: otherRoot,
      issue: otherRoot,
      role: "planner",
    };
    const { manager: processes, state: managedState } = manager(state, {
      config: config(stateDir),
    });

    await processes.closeTree(root);

    expect(managedState.trees[root]?.status).toBe("closed");
    expect(managedState.spawnCapabilities[spawnCapabilityKey(closingSpawnToken)]).toBeUndefined();
    // A capability for an unrelated, still-active tree must survive this tree's own close.
    expect(managedState.spawnCapabilities[spawnCapabilityKey(unrelatedSpawnToken)]).toEqual({
      tree: otherRoot,
      issue: otherRoot,
      role: "planner",
    });
  });

  it("refuses to spawn or resume a worker on a tree that is mid-teardown, with a 409-mapped TreeClosingError", async () => {
    const state = newLegionState("omp", 1);
    tree(state);
    const stuckClient = fakeWorkerRpcClient();
    stuckClient.shutdown = () => {};
    const { manager: processes } = manager(state, {
      sleep: async () => {},
      connectWorkerRpc: async () => stuckClient,
    });

    // Not awaited: `closeTree` runs synchronously up to its first internal await (inside
    // `probe`), so `closingTrees` is already populated by the time this line returns control.
    const closePromise = processes.closeTree(root);

    const error = await processes
      .spawnWorker(root, root, "tester", "verify #41")
      .catch((caught: unknown) => caught);

    expect(error).toBeInstanceOf(TreeClosingError);
    expect((error as TreeClosingError).treeKey).toBe(root);

    await closePromise;
  });

  it("refuses to deliver a worker/ready queued assignment on a tree that is mid-teardown", async () => {
    const state = newLegionState("omp", 1);
    tree(state);
    state.roles[roleToken("omp", root, "tester")] = {
      issue: root,
      role: "tester",
      generation: 1,
      sessionId: "ses_tester",
      pendingAssignment: "verify #41",
      locator: {
        tmuxSession: "legion-omp",
        tmuxWindowId: "@42",
        tmuxPaneId: "%7",
        socketPath: "/state/workers/tester.sock",
      },
    };
    const stuckClient = fakeWorkerRpcClient();
    stuckClient.shutdown = () => {};
    const readyClient = fakeWorkerRpcClient();
    const { manager: processes } = manager(state, {
      sleep: async () => {},
      connectWorkerRpc: async (socketPath) =>
        socketPath === "/state/workers/tester.sock" ? readyClient : stuckClient,
    });

    const closePromise = processes.closeTree(root);

    const error = await processes
      .workerReady(root, "tester", "ses_tester", 1)
      .catch((caught: unknown) => caught);

    expect(error).toBeInstanceOf(TreeClosingError);
    expect((error as TreeClosingError).treeKey).toBe(root);
    expect(readyClient.prompts).toEqual([]);

    await closePromise;
  });

  it("rejects mutateLiveRoleClaim (the fence /worker/started's own claim write runs behind) once closeTree has already deleted this token's claim under the same per-role lock", async () => {
    const state = newLegionState("omp", 1);
    tree(state);
    const token = roleToken("omp", root, "tester");
    state.roles[token] = {
      issue: root,
      role: "tester",
      generation: 1,
      sessionId: "ses_tester",
      locator: {
        tmuxSession: "legion-omp",
        tmuxWindowId: "@42",
        tmuxPaneId: "%7",
        socketPath: "/state/workers/tester.sock",
      },
    };
    const { manager: processes, state: managedState } = manager(state, { sleep: async () => {} });

    await processes.closeTree(root);
    expect(managedState.trees[root]?.status).toBe("closed");
    expect(managedState.roles[token]).toBeUndefined();

    let fnCalled = false;
    const error = await processes
      .mutateLiveRoleClaim(root, root, token, async () => {
        fnCalled = true;
      })
      .catch((caught: unknown) => caught);

    // Rejected at entry -- before ever touching the per-role lock or `fn` -- exactly like
    // spawnWorker/workerReady already do for a tree that is closed by the time the request
    // arrives, not merely one still tearing down.
    expect(error).toBeInstanceOf(TreeClosingError);
    expect((error as TreeClosingError).treeKey).toBe(root);
    expect(fnCalled).toBe(false);
  });

  it("rejects mutateLiveRoleClaim queued behind an in-progress closeTree for the same token, never running fn once that close has deleted the claim", async () => {
    const state = newLegionState("omp", 1);
    tree(state);
    const token = roleToken("omp", root, "tester");
    state.roles[token] = {
      issue: root,
      role: "tester",
      generation: 1,
      sessionId: "ses_tester",
      locator: {
        tmuxSession: "legion-omp",
        tmuxWindowId: "@42",
        tmuxPaneId: "%7",
        socketPath: "/state/workers/tester.sock",
      },
    };
    const stopGate = Promise.withResolvers<void>();
    const client = fakeWorkerRpcClient();
    client.shutdown = () => {
      void stopGate.promise.then(() => client.close());
    };
    const { manager: processes, state: managedState } = manager(state, {
      sleep: async () => {},
      connectWorkerRpc: async () => client,
    });

    // closeTree marks `closingTrees` synchronously as its very first act, before ever touching
    // this token's own critical section (its stop-then-delete only follows once the gated
    // `shutdown()` below resolves) -- so a call arriving any time after this line, on this same
    // per-token lock `closeTreeLocked`'s delete also uses, is rejected by the entry check alone,
    // consistent with every other write this fence protects (`spawnWorker`, `workerReady`).
    const closePromise = processes.closeTree(root);

    let fnCalled = false;
    const mutatePromise = processes
      .mutateLiveRoleClaim(root, root, token, async () => {
        fnCalled = true;
      })
      .catch((caught: unknown) => caught);

    stopGate.resolve();
    const error = await mutatePromise;
    await closePromise;

    expect(error).toBeInstanceOf(TreeClosingError);
    expect(fnCalled).toBe(false);
    expect(managedState.trees[root]?.status).toBe("closed");
    expect(managedState.roles[token]).toBeUndefined();
  });

  it("delivers a worker's pending assignment over its socket on worker/ready, clears it, and resets launchFailures", async () => {
    const state = newLegionState("omp", 1);
    const token = roleToken("omp", root, "tester");
    state.roles[token] = {
      issue: root,
      role: "tester",
      generation: 1,
      sessionId: "ses_tester",
      pendingAssignment: "verify #41",
      // A prior generation's unconfirmed boot(s) left this behind; a durable ready
      // confirmation is the only thing that ever resets it (never mere registration).
      launchFailures: 2,
      locator: {
        tmuxSession: "legion-omp",
        tmuxWindowId: "@42",
        tmuxPaneId: "%7",
        socketPath: "/state/workers/tester.sock",
      },
    };
    const client = fakeWorkerRpcClient();
    const { manager: processes, state: managedState } = manager(state, {
      connectWorkerRpc: async () => client,
    });

    await processes.workerReady(root, "tester", "ses_tester", 1);

    expect(client.prompts).toEqual(["verify #41"]);
    const claim = managedState.roles[token];
    if (!claim || !("issue" in claim)) throw new Error("worker claim disappeared");
    expect(claim.pendingAssignment).toBeUndefined();
    expect(claim.launchFailures).toBeUndefined();
    // Same fix as the resumed-live-socket branch of spawnWorker: a delivered pending assignment
    // must re-register as the issue's active phase, or the worker's eventual `handoff complete`
    // 409s against a phase this delivery path never restored.
    expect(managedState.phases[root]).toEqual({ phase: "tester", sessionId: "ses_tester" });
  });

  it("resets launchFailures once worker/ready durably confirms a claim with no pending assignment", async () => {
    const state = newLegionState("omp", 1);
    const token = roleToken("omp", root, "tester");
    state.roles[token] = {
      issue: root,
      role: "tester",
      generation: 1,
      sessionId: "ses_tester",
      launchFailures: 2,
      locator: {
        tmuxSession: "legion-omp",
        tmuxWindowId: "@42",
        tmuxPaneId: "%7",
        socketPath: "/state/workers/tester.sock",
      },
    };
    const client = fakeWorkerRpcClient();
    const { manager: processes, state: managedState } = manager(state, {
      connectWorkerRpc: async () => client,
    });

    await processes.workerReady(root, "tester", "ses_tester", 1);

    expect(client.prompts).toEqual([]);
    const claim = managedState.roles[token];
    if (!claim || !("issue" in claim)) throw new Error("worker claim disappeared");
    expect(claim.readyConfirmedAt).toBeNumber();
    expect(claim.launchFailures).toBeUndefined();
  });
  it("serializes concurrent worker/ready calls so the pending assignment prompts once", async () => {
    const state = newLegionState("omp", 1);
    const token = roleToken("omp", root, "tester");
    state.roles[token] = {
      issue: root,
      role: "tester",
      generation: 1,
      sessionId: "ses_tester",
      pendingAssignment: "verify #41",
      locator: {
        tmuxSession: "legion-omp",
        tmuxWindowId: "@42",
        tmuxPaneId: "%7",
        socketPath: "/state/workers/tester.sock",
      },
    };
    const client = fakeWorkerRpcClient();
    const originalPrompt = client.prompt;
    const promptStarted = Promise.withResolvers<void>();
    const releasePrompt = Promise.withResolvers<void>();
    client.prompt = async (task) => {
      promptStarted.resolve();
      await releasePrompt.promise;
      await originalPrompt(task);
    };
    const { manager: processes, state: managedState } = manager(state, {
      connectWorkerRpc: async () => client,
    });

    const first = processes.workerReady(root, "tester", "ses_tester", 1);
    await promptStarted.promise;
    const second = processes.workerReady(root, "tester", "ses_tester", 1);
    releasePrompt.resolve();
    await Promise.all([first, second]);

    expect(client.prompts).toEqual(["verify #41"]);
    const claim = managedState.roles[token];
    if (!claim || !("issue" in claim)) throw new Error("worker claim disappeared");
    expect(claim.pendingAssignment).toBeUndefined();
    expect(claim.readyConfirmedAt).toBeNumber();
  });

  it("ignores worker/ready from a stale generation even when the session id matches, leaving pendingAssignment intact", async () => {
    const state = newLegionState("omp", 1);
    const token = roleToken("omp", root, "tester");
    state.roles[token] = {
      issue: root,
      role: "tester",
      generation: 2,
      sessionId: "ses_tester",
      pendingAssignment: "verify #55",
      locator: {
        tmuxSession: "legion-omp",
        tmuxWindowId: "@42",
        tmuxPaneId: "%7",
        socketPath: "/state/workers/tester.sock",
      },
    };
    const client = fakeWorkerRpcClient();
    const { manager: processes, state: managedState } = manager(state, {
      connectWorkerRpc: async () => client,
    });

    // A same-agent respawn keeps the session id, so a late worker/ready from the replaced
    // (generation 1) process must not be able to consume generation 2's pending assignment.
    await processes.workerReady(root, "tester", "ses_tester", 1);

    expect(client.prompts).toEqual([]);
    const claim = managedState.roles[token];
    if (!claim || !("issue" in claim)) throw new Error("worker claim disappeared");
    expect(claim.pendingAssignment).toBe("verify #55");
  });

  it("treats a saveState failure after a successful prompt as a persistence issue, not a prompt failure: keeps the delivered phase and cleared pendingAssignment regardless", async () => {
    const state = newLegionState("omp", 1);
    const token = roleToken("omp", root, "tester");
    state.roles[token] = {
      issue: root,
      role: "tester",
      generation: 1,
      sessionId: "ses_tester",
      pendingAssignment: "verify #41",
      locator: {
        tmuxSession: "legion-omp",
        tmuxWindowId: "@42",
        tmuxPaneId: "%7",
        socketPath: "/state/workers/tester.sock",
      },
    };
    const client = fakeWorkerRpcClient();
    let saveStateCalls = 0;
    const { manager: processes, state: managedState } = manager(state, {
      connectWorkerRpc: async () => client,
      saveState: async () => {
        saveStateCalls += 1;
        // Both the original persist and its one retry fail here — mirroring launchWorker's
        // post-locator-write save handling (see the "treats a saveState failure after a
        // successful launch..." test above): the prompt itself already succeeded (the worker
        // is already working), so this must never surface as a failure to the caller.
        throw new Error("disk full");
      },
    });

    await processes.workerReady(root, "tester", "ses_tester", 1);

    expect(client.prompts).toEqual(["verify #41"]);
    expect(saveStateCalls).toBe(2);
    const claim = managedState.roles[token];
    if (!claim || !("issue" in claim)) throw new Error("worker claim disappeared");
    // The in-memory claim remains authoritative even though both persist attempts failed: the
    // phase registration and cleared pendingAssignment stay exactly as the successful prompt
    // left them, never rolled back.
    expect(claim.pendingAssignment).toBeUndefined();
    expect(claim.promptFailures).toBe(0);
    expect(managedState.phases[root]).toEqual({ phase: "tester", sessionId: "ses_tester" });
  });

  it("reconnects to every worker claim with a locator on daemon start", async () => {
    const state = newLegionState("omp", 1);
    state.roles[roleToken("omp", root, "planner")] = {
      issue: root,
      role: "planner",
      locator: {
        tmuxSession: "legion-omp",
        tmuxWindowId: "@42",
        tmuxPaneId: "%1",
        socketPath: "/state/workers/planner.sock",
      },
    };
    state.roles[roleToken("omp", root, "tester")] = {
      issue: root,
      role: "tester",
      locator: {
        tmuxSession: "legion-omp",
        tmuxWindowId: "@42",
        tmuxPaneId: "%2",
        socketPath: "/state/workers/tester.sock",
      },
    };
    const connectedSockets: string[] = [];
    const { manager: processes } = manager(state, {
      connectWorkerRpc: async (socketPath) => {
        connectedSockets.push(socketPath);
        return fakeWorkerRpcClient();
      },
    });

    await processes.reconnectWorkers();

    expect(connectedSockets.sort()).toEqual(
      ["/state/workers/planner.sock", "/state/workers/tester.sock"].sort()
    );
  });

  it("does not count an idle worker (isStreaming: false) toward the running-worker cap after reconnectWorkers", async () => {
    const stateDir = await temporaryDir();
    const workspace = path.join(stateDir, "workspaces", "sjawhar", "legion", "issue-42");
    await mkdir(workspace, { recursive: true });
    const state = newLegionState("omp", 1);
    tree(state);
    const idleToken = roleToken("omp", root, "planner");
    state.roles[idleToken] = {
      issue: root,
      role: "planner",
      sessionId: "ses_planner",
      locator: {
        tmuxSession: "legion-omp",
        tmuxWindowId: "@42",
        tmuxPaneId: "%1",
        socketPath: "/state/workers/planner.sock",
      },
    };
    const idleClient = fakeWorkerRpcClient();
    // Models the real client's getState(): interprets a false isStreaming into an idle
    // transition, exactly as reconnectWorkers relies on to seed runState from a fresh
    // reconnection instead of leaving it stuck at the conservative "unknown" default.
    idleClient.getStateImpl = async () => {
      idleClient.emitRunState("idle");
      return { data: { isStreaming: false } };
    };
    const { manager: processes, commands } = manager(state, {
      config: config(stateDir, { workerCap: 1 }),
      connectWorkerRpc: async () => idleClient,
    });

    await processes.reconnectWorkers();
    expect(idleClient.getStateCalls).toBe(1);
    expect(idleClient.runState).toBe("idle");

    // The cap is 1 and the only existing claim is idle (not counted by runningWorkerCount): a
    // fresh spawn for a different role must be admitted immediately, not queued behind a
    // phantom occupant.
    const result = await processes.spawnWorker(root, root, "tester", "verify #41");
    expect(result.status).toBe("spawned");
    expect(
      commands.some((command) => command[0] === "tmux" && command[1] === "new-window")
    ).toBeTrue();
  });

  it("closes and does not cache a worker socket whose negotiation fails, so a later call reconnects", async () => {
    const state = newLegionState("omp", 1);
    const token = roleToken("omp", root, "tester");
    state.roles[token] = {
      issue: root,
      role: "tester",
      generation: 1,
      sessionId: "ses_tester",
      pendingAssignment: "verify #41",
      locator: {
        tmuxSession: "legion-omp",
        tmuxWindowId: "@42",
        tmuxPaneId: "%7",
        socketPath: "/state/workers/tester.sock",
      },
    };
    let connectCalls = 0;
    let firstClientClosed = false;
    const goodClient = fakeWorkerRpcClient();
    const { manager: processes } = manager(state, {
      connectWorkerRpc: async () => {
        connectCalls += 1;
        if (connectCalls === 1) {
          return {
            ...fakeWorkerRpcClient(),
            negotiate: async () => {
              throw new Error("shim never answered negotiate_protocol");
            },
            close: () => {
              firstClientClosed = true;
            },
          };
        }
        return goodClient;
      },
    });

    await expect(processes.workerReady(root, "tester", "ses_tester", 1)).rejects.toThrow(
      "shim never answered negotiate_protocol"
    );
    expect(firstClientClosed).toBe(true);

    await processes.workerReady(root, "tester", "ses_tester", 1);

    expect(connectCalls).toBe(2);
    expect(goodClient.prompts).toEqual(["verify #41"]);
  });

  it("clears a claim's stale locator when reconnectWorkers finds its socket dead, keeping its pending assignment", async () => {
    const state = newLegionState("omp", 1);
    const token = roleToken("omp", root, "tester");
    state.roles[token] = {
      issue: root,
      role: "tester",
      pendingAssignment: "verify #41",
      locator: {
        tmuxSession: "legion-omp",
        tmuxWindowId: "@42",
        tmuxPaneId: "%7",
        socketPath: "/state/workers/dead-tester.sock",
      },
    };
    const { manager: processes, state: managedState } = manager(state, {
      connectWorkerRpc: async () => {
        throw new Error("ECONNREFUSED");
      },
    });

    await processes.reconnectWorkers();

    const claim = managedState.roles[token];
    if (!claim || !("issue" in claim)) throw new Error("worker claim disappeared");
    expect(claim.locator).toBeUndefined();
    expect(claim.pendingAssignment).toBe("verify #41");
  });

  it("passes DISPATCH_URL and DISPATCH_TOKEN to a spawned phase worker, never the retired DISPATCH_MCP_URL alias", async () => {
    const stateDir = await temporaryDir();
    const workspace = path.join(stateDir, "workspaces", "sjawhar", "legion", "issue-42");
    await mkdir(workspace, { recursive: true });
    const state = newLegionState("omp", 1);
    state.trees[root] = {
      root,
      generation: 1,
      status: "active",
      launchFailures: 0,
    };
    const { manager: processes, commands } = manager(state, {
      config: config(stateDir, {
        dispatchUrl: "http://127.0.0.1:18766",
        dispatchToken: "test-dispatch-token",
      }),
      run: async (command) => {
        commands.push(command);
        if (command[0] === "tmux" && command[1] === "new-window") {
          return { stdout: "@99 %201 12345\n", exitCode: 0 };
        }
        return { stdout: "", exitCode: 0 };
      },
    });

    await processes.spawnWorker(root, root, "tester", "verify #41");

    const windowCommand = commands.find(
      (command) => command[0] === "tmux" && command[1] === "new-window" && command.includes("-n")
    );
    if (!windowCommand) throw new Error("worker spawn did not open a tmux window");
    const environment = tmuxWindowEnvironment(windowCommand);
    expect(environment.DISPATCH_URL).toBe("http://127.0.0.1:18766");
    expect(environment.DISPATCH_TOKEN).toBe("test-dispatch-token");
    expect(environment.DISPATCH_MCP_URL).toBeUndefined();
  });

  it("passes DISPATCH_URL and DISPATCH_TOKEN to the controller pane, never the retired DISPATCH_MCP_URL alias", async () => {
    const stateDir = await temporaryDir();
    const { manager: processes, commands } = manager(newLegionState("omp", 1), {
      config: config(stateDir, {
        dispatchUrl: "http://127.0.0.1:18766",
        dispatchToken: "test-dispatch-token",
      }),
    });

    await processes.ensureController();

    const windowCommand = commands.find(
      (command) => command[0] === "tmux" && command[1] === "new-window"
    );
    if (!windowCommand) throw new Error("controller spawn did not open a tmux window");
    const environment = tmuxWindowEnvironment(windowCommand);
    expect(environment.DISPATCH_URL).toBe("http://127.0.0.1:18766");
    expect(environment.DISPATCH_TOKEN).toBe("test-dispatch-token");
    expect(environment.DISPATCH_MCP_URL).toBeUndefined();
  });

  it("kills a still-running pane whose socket is unreachable before clearing its locator on reconnect", async () => {
    const state = newLegionState("omp", 1);
    const token = roleToken("omp", root, "tester");
    state.roles[token] = {
      issue: root,
      role: "tester",
      pendingAssignment: "verify #41",
      locator: {
        tmuxSession: "legion-omp",
        tmuxWindowId: "@42",
        tmuxPaneId: "%7",
        socketPath: "/state/workers/dead-tester.sock",
      },
    };
    const commands: string[][] = [];
    const { manager: processes, state: managedState } = manager(state, {
      sleep: async () => {},
      connectWorkerRpc: async () => {
        throw new Error("ECONNREFUSED");
      },
      run: async (command) => {
        commands.push(command);
        if (
          command[0] === "tmux" &&
          command[1] === "list-panes" &&
          command.includes("%7") &&
          command.includes("#{pane_pid}")
        ) {
          return { stdout: "22222\n", exitCode: 0 };
        }
        return { stdout: "", exitCode: 0 };
      },
    });

    await processes.reconnectWorkers();

    expect(
      commands.some(
        (command) => command[0] === "tmux" && command[1] === "kill-pane" && command.includes("%7")
      )
    ).toBeTrue();
    const claim = managedState.roles[token];
    if (!claim || !("issue" in claim)) throw new Error("worker claim disappeared");
    expect(claim.locator).toBeUndefined();
    expect(claim.pendingAssignment).toBe("verify #41");
  });

  it("bails without connecting again or killing when the current claim's locator no longer matches the one that was probed", async () => {
    const state = newLegionState("omp", 1);
    const token = roleToken("omp", root, "tester");
    const staleLocator = {
      tmuxSession: "legion-omp",
      tmuxWindowId: "@42",
      tmuxPaneId: "%7",
      socketPath: "/state/workers/tester.sock",
    };
    const freshLocator = {
      tmuxSession: "legion-omp",
      tmuxWindowId: "@50",
      tmuxPaneId: "%9",
      socketPath: "/state/workers/tester.sock",
    };
    state.roles[token] = {
      issue: root,
      role: "tester",
      pendingAssignment: "verify #41",
      locator: staleLocator,
    };
    const connectAttempts: string[] = [];
    const commands: string[][] = [];
    const { manager: processes, state: managedState } = manager(state, {
      connectWorkerRpc: async (socketPath) => {
        connectAttempts.push(socketPath);
        // A respawn completes and replaces this claim's locator with a fresh pane id while the
        // probe is still in flight -- mirrors a concurrent spawnWorker finishing between this
        // probe starting and its failure being handled.
        const current = state.roles[token];
        if (current && "issue" in current) current.locator = freshLocator;
        throw new Error("ECONNREFUSED");
      },
      run: async (command) => {
        commands.push(command);
        return { stdout: "", exitCode: 0 };
      },
    });

    await processes.reconnectWorkers();

    // Exactly the one probe connect -- the identity mismatch bails before `stopClient` ever
    // gets a chance to connect (which would otherwise reach the socket path a respawn reuses).
    expect(connectAttempts).toEqual(["/state/workers/tester.sock"]);
    expect(commands.filter((c) => c[1] === "kill-pane")).toEqual([]);
    const claim = managedState.roles[token];
    if (!claim || !("issue" in claim)) throw new Error("worker claim disappeared");
    expect(claim.locator).toEqual(freshLocator);
  });

  it("reconnectWorkers enqueues a dead unconfirmed claim for the normal admission drain, never launching it directly at restart before promotion is enabled", async () => {
    const stateDir = await temporaryDir();
    const workspace = path.join(stateDir, "workspaces", "sjawhar", "legion", "issue-42");
    await mkdir(workspace, { recursive: true });
    const resumeFile = path.join(stateDir, "prior-implementer-session.json");
    await writeFile(resumeFile, "{}", "utf8");
    const state = newLegionState("omp", 1);
    tree(state);
    state.issues[child] = {
      key: child,
      title: "Child",
      status: "in_progress",
      parent: root,
      children: [],
    };
    const token = roleToken("omp", child, "implementer");
    state.roles[token] = {
      issue: child,
      role: "implementer",
      generation: 1,
      pendingAssignment: "implement #43",
      locator: {
        tmuxSession: "legion-omp",
        tmuxWindowId: "@42",
        tmuxPaneId: "%9",
        socketPath: "/state/workers/dead-implementer.sock",
        ompSessionFile: resumeFile,
      },
    };
    const commands: string[][] = [];
    // `skipEnablePromotion` models exactly what `reconnectWorkers` runs under in production: the
    // daemon calls it before `api` exists, hence before `enableWorkerPromotion()`.
    const { manager: processes, state: managedState } = manager(
      state,
      {
        config: config(stateDir, { workerCap: 1 }),
        connectWorkerRpc: async () => {
          throw new Error("ECONNREFUSED");
        },
        run: async (command) => {
          commands.push(command);
          if (command[0] === "tmux" && command[1] === "list-panes") {
            // Reports the original dead pane as gone (drives the first, restart-time
            // retirement decision); the freshly-launched pane's own later liveness is never
            // queried by this test.
            return { stdout: "", exitCode: 1 };
          }
          if (command[0] === "tmux" && command[1] === "new-window") {
            return { stdout: "@50 %50 54321\n", exitCode: 0 };
          }
          return { stdout: "", exitCode: 0 };
        },
      },
      { skipEnablePromotion: true }
    );

    await processes.reconnectWorkers();

    // Never launched directly: no tmux command at all beyond the pane-liveness poll
    // `retireWorkerLocator` itself makes (which reports dead here) — specifically no
    // `new-window`/`split-window` opening a fresh pane.
    expect(
      commands.some((command) => command[1] === "new-window" || command[1] === "split-window")
    ).toBeFalse();
    expect(managedState.workerAdmission.queue).toEqual([token]);
    const queuedClaim = managedState.roles[token];
    if (!queuedClaim || !("issue" in queuedClaim)) throw new Error("claim disappeared");
    expect(queuedClaim.locator).toBeUndefined();
    expect(queuedClaim.resumeSessionFile).toBe(resumeFile);
    expect(queuedClaim.launchFailures).toBe(1);
    expect(queuedClaim.pendingAssignment).toBe("implement #43");

    // Only once promotion is enabled (the post-`api`-assignment boot step) does the queued
    // retry actually launch, through the normal cap-aware, role-locked drain.
    processes.enableWorkerPromotion();
    commands.length = 0;
    await processes.reconcileWorkerAdmission();

    expect(commands.some((command) => command[1] === "new-window")).toBeTrue();
    expect(managedState.workerAdmission.queue).toEqual([]);
    const launchedClaim = managedState.roles[token];
    if (!launchedClaim || !("issue" in launchedClaim)) throw new Error("claim disappeared");
    expect(launchedClaim.locator).toBeDefined();
  });

  it("persists a retirement's queue-push in the same save as its locator-clear, so a reload after a crash mid-drain still finds the token queued", async () => {
    const stateDir = await temporaryDir();
    const stateFile = path.join(stateDir, "state.json");
    const resumeFile = path.join(stateDir, "prior-implementer-session.json");
    await writeFile(resumeFile, "{}", "utf8");
    const state = newLegionState("omp", 1);
    tree(state);
    state.issues[child] = {
      key: child,
      title: "Child",
      status: "in_progress",
      parent: root,
      children: [],
    };
    const token = roleToken("omp", child, "implementer");
    state.roles[token] = {
      issue: child,
      role: "implementer",
      generation: 1,
      pendingAssignment: "implement #43",
      locator: {
        tmuxSession: "legion-omp",
        tmuxWindowId: "@42",
        tmuxPaneId: "%9",
        socketPath: "/state/workers/dead-implementer.sock",
        ompSessionFile: resumeFile,
      },
    };
    const { manager: processes } = manager(
      state,
      {
        config: config(stateDir, { workerCap: 1 }),
        // The real save/load round trip, exactly as `index.ts` wires it — this test's whole
        // point is proving what actually lands on disk, not just what the in-memory object
        // holds afterward.
        saveState: () => legionStateSaveState(stateFile, state),
        connectWorkerRpc: async () => {
          throw new Error("ECONNREFUSED");
        },
        run: async (command) => {
          if (command[0] === "tmux" && command[1] === "list-panes") {
            return { stdout: "", exitCode: 1 };
          }
          return { stdout: "", exitCode: 0 };
        },
      },
      { skipEnablePromotion: true }
    );

    await processes.reconnectWorkers();

    const reloaded = await legionStateLoadState(stateFile, { project: "omp", cap: 1 });
    expect(reloaded.workerAdmission.queue).toEqual([token]);
    const reloadedClaim = reloaded.roles[token];
    if (!reloadedClaim || !("issue" in reloadedClaim)) throw new Error("claim disappeared");
    expect(reloadedClaim.locator).toBeUndefined();
    expect(reloadedClaim.resumeSessionFile).toBe(resumeFile);
    expect(reloadedClaim.launchFailures).toBe(1);
    expect(reloadedClaim.pendingAssignment).toBe("implement #43");

    // Promotion, once enabled against the *reloaded* state, finds the token still queued and
    // relaunches it — proving the persisted queue entry is actually usable, not merely present.
    const relaunchCommands: string[][] = [];
    const { manager: reloadedProcesses, state: reloadedManagedState } = manager(reloaded, {
      config: config(stateDir, { workerCap: 1 }),
      run: async (command) => {
        relaunchCommands.push(command);
        if (command[0] === "tmux" && command[1] === "new-window") {
          return { stdout: "@50 %50 54321\n", exitCode: 0 };
        }
        return { stdout: "", exitCode: 0 };
      },
    });
    reloadedProcesses.enableWorkerPromotion();
    await reloadedProcesses.reconcileWorkerAdmission();

    expect(relaunchCommands.some((command) => command[1] === "new-window")).toBeTrue();
    expect(reloadedManagedState.workerAdmission.queue).toEqual([]);
  });

  it("a spawnWorker call racing a runtime retirement decision for the same unconfirmed token, at cap 1, produces exactly one pane and one coherent claim", async () => {
    const stateDir = await temporaryDir();
    const state = newLegionState("omp", 1);
    tree(state);
    const resumeFile = path.join(stateDir, "prior-implementer-session.json");
    await writeFile(resumeFile, "{}", "utf8");
    const token = roleToken("omp", root, "implementer");
    state.roles[token] = {
      issue: root,
      role: "implementer",
      generation: 1,
      launchFailures: 0,
      locator: {
        tmuxSession: "legion-omp",
        tmuxWindowId: "@42",
        tmuxPaneId: "%9",
        socketPath: "/state/workers/dead-implementer.sock",
        ompSessionFile: resumeFile,
      },
    };
    const commands: string[][] = [];
    const publications: Array<{ subject: string; json: string }> = [];
    const relaunched = Promise.withResolvers<void>();
    const { manager: processes, state: managedState } = manager(state, {
      config: config(stateDir, { workerCap: 1 }),
      connectWorkerRpc: async () => {
        throw new Error("ECONNREFUSED");
      },
      natsPublish: (subject, json) => {
        publications.push({ subject, json });
        if (
          subject === roleTopic(roleToken("omp", root, "architect")) &&
          json === JSON.stringify({ type: "worker-started", issue: root, role: "implementer" })
        ) {
          relaunched.resolve();
        }
      },
      run: async (command) => {
        commands.push(command);
        if (command[0] === "tmux" && command[1] === "list-panes") {
          // The recorded pane is gone in every liveness probe throughout this test.
          return { stdout: "", exitCode: 1 };
        }
        if (
          command[0] === "tmux" &&
          (command[1] === "new-window" || command[1] === "split-window")
        ) {
          return { stdout: "@50 %50 54321\n", exitCode: 0 };
        }
        return { stdout: "", exitCode: 0 };
      },
    });
    processes.enableWorkerPromotion();

    // Races a runtime retirement decision (as `reconnectWorkers`, the boot watchdog, and
    // `onWorkerClientClosed` all funnel through `retireUnconfirmedBoot`) against a concurrent
    // `spawnWorker` call for the *same* token. Both serialize through the same
    // `WorkerAdmission.mutateClaim(token, …)` per-token queue, so exactly one of them runs
    // first — but either order must still yield exactly one pane: if `spawnWorker` wins the
    // queue, it sees the still-unconfirmed claim and queues the new task without launching
    // (the "booting" branch); if the retirement wins, it clears the locator and enqueues,
    // leaving `spawnWorker` to see no locator and launch fresh. A bug that let both sides
    // decide to launch independently would open two panes at a cap of one.
    await Promise.all([
      processes.reconnectWorkers(),
      processes.spawnWorker(root, root, "implementer", "task2"),
    ]);
    await relaunched.promise;

    const paneOpens = commands.filter(
      (command) => command[1] === "new-window" || command[1] === "split-window"
    );
    expect(paneOpens.length).toBe(1);
    expect(publications.filter((publication) => publication.json.includes("worker-died"))).toEqual(
      []
    );

    const claim = managedState.roles[token];
    if (!claim || !("issue" in claim)) throw new Error("claim disappeared");
    expect(claim.locator).toBeDefined();
    expect(claim.locator?.tmuxPaneId).toBe("%50");
    expect(claim.pendingAssignment).toBe("task2");
    expect(claim.resumeSessionFile ?? claim.locator?.ompSessionFile).toBe(resumeFile);
  });

  it("a worker-ready confirmation whose lock acquisition wins a race against a runtime retirement decision keeps the claim confirmed, making the retirement a no-op", async () => {
    const stateDir = await temporaryDir();
    const state = newLegionState("omp", 1);
    tree(state);
    const resumeFile = path.join(stateDir, "prior-implementer-session.json");
    await writeFile(resumeFile, "{}", "utf8");
    const token = roleToken("omp", root, "implementer");
    state.roles[token] = {
      issue: root,
      role: "implementer",
      generation: 3,
      locator: {
        tmuxSession: "legion-omp",
        tmuxWindowId: "@42",
        tmuxPaneId: "%9",
        socketPath: "/state/workers/implementer.sock",
        ompSessionFile: resumeFile,
      },
    };
    const capturedGeneration = 3;
    const capturedPaneId = "%9";
    const commands: string[][] = [];
    const { manager: processes, state: managedState } = manager(state, {
      config: config(stateDir, { workerCap: 1 }),
      connectWorkerRpc: async () => {
        throw new Error("ECONNREFUSED");
      },
      run: async (command) => {
        commands.push(command);
        if (command[0] === "tmux" && command[1] === "list-panes")
          return { stdout: "", exitCode: 1 };
        return { stdout: "", exitCode: 0 };
      },
    });
    processes.enableWorkerPromotion();

    // Models `workerReady`'s atomic confirmation state change. Called with no preceding await,
    // its `mutateClaim` registration lands on the per-token queue essentially immediately —
    // ahead of `reconnectWorkers`' own retirement decision, which only reaches its `mutateClaim`
    // call after its `workerClient` connect attempt rejects (several microtask ticks later) —
    // so this wins the race to run first.
    const confirm = () =>
      processes.mutateLiveRoleClaim(root, root, token, async () => {
        const current = managedState.roles[token];
        if (
          !current ||
          !("issue" in current) ||
          !current.locator ||
          current.generation !== capturedGeneration ||
          current.locator.tmuxPaneId !== capturedPaneId
        ) {
          throw new Error("Stale worker generation");
        }
        // Replaces the object (never mutates it in place), exactly like the real ready path
        // does — so `reconnectWorkers`' claim reference, captured by value before confirmation,
        // stays stale and this test genuinely exercises `retireUnconfirmedBoot`'s fresh re-read.
        managedState.roles[token] = {
          ...current,
          sessionId: "ses_confirmed",
          readyConfirmedAt: Date.parse("2026-08-24T00:00:00.000Z"),
        };
        return "confirmed";
      });

    const [reconnectResult, confirmResult] = await Promise.allSettled([
      processes.reconnectWorkers(),
      confirm(),
    ]);

    expect(reconnectResult.status).toBe("fulfilled");
    expect(confirmResult.status).toBe("fulfilled");
    if (confirmResult.status === "fulfilled") expect(confirmResult.value).toBe("confirmed");

    const claim = managedState.roles[token];
    if (!claim || !("issue" in claim)) throw new Error("claim disappeared");
    expect(claim.sessionId).toBe("ses_confirmed");
    expect(claim.locator?.tmuxPaneId).toBe("%9");
    expect(claim.generation).toBe(3);
    expect(claim.launchFailures).toBeUndefined();
    expect(commands.some((command) => command[1] === "kill-pane")).toBeFalse();
  });

  it("a runtime retirement decision that wins its lock acquisition before a racing worker registration's slow GitHub-lease fetch resolves rejects the stale registration and leaves the claim retired for retry", async () => {
    const stateDir = await temporaryDir();
    const state = newLegionState("omp", 1);
    tree(state);
    const resumeFile = path.join(stateDir, "prior-implementer-session.json");
    await writeFile(resumeFile, "{}", "utf8");
    const token = roleToken("omp", root, "implementer");
    state.roles[token] = {
      issue: root,
      role: "implementer",
      generation: 3,
      locator: {
        tmuxSession: "legion-omp",
        tmuxWindowId: "@42",
        tmuxPaneId: "%9",
        socketPath: "/state/workers/implementer.sock",
        ompSessionFile: resumeFile,
      },
    };
    const capturedGeneration = 3;
    const capturedPaneId = "%9";
    const commands: string[][] = [];
    const { manager: processes, state: managedState } = manager(state, {
      config: config(stateDir, { workerCap: 1 }),
      connectWorkerRpc: async () => {
        throw new Error("ECONNREFUSED");
      },
      run: async (command) => {
        commands.push(command);
        if (command[0] === "tmux" && command[1] === "list-panes")
          return { stdout: "", exitCode: 1 };
        return { stdout: "", exitCode: 0 };
      },
    });

    // Simulates `handleWorkerStarted`'s slow, deliberately-outside-any-lock GitHub lease fetch:
    // a real macrotask tick flushes every pending microtask first, so `reconnectWorkers`' own
    // `mutateClaim` registration (a handful of microtask ticks past its rejected connect
    // attempt) always lands on the per-token queue — and therefore commits — before this
    // confirmation's lock acquisition is even attempted.
    const confirm = async () => {
      await new Promise((resolve) => setTimeout(resolve, 0));
      return processes.mutateLiveRoleClaim(root, root, token, async () => {
        const current = managedState.roles[token];
        if (
          !current ||
          !("issue" in current) ||
          !current.locator ||
          current.generation !== capturedGeneration ||
          current.locator.tmuxPaneId !== capturedPaneId
        ) {
          throw new Error("Stale worker generation");
        }
        managedState.roles[token] = { ...current, sessionId: "ses_confirmed" };
        return "confirmed";
      });
    };

    const [reconnectResult, confirmResult] = await Promise.allSettled([
      processes.reconnectWorkers(),
      confirm(),
    ]);

    expect(reconnectResult.status).toBe("fulfilled");
    expect(confirmResult.status).toBe("rejected");
    if (confirmResult.status === "rejected") {
      expect(String(confirmResult.reason)).toContain("Stale worker generation");
    }

    const claim = managedState.roles[token];
    if (!claim || !("issue" in claim)) throw new Error("claim disappeared");
    expect(claim.sessionId).toBeUndefined();
    expect(claim.locator).toBeUndefined();
    expect(claim.launchFailures).toBe(1);
    expect(claim.resumeSessionFile ?? "").toBe(resumeFile);
  });

  it("swallows a controller shim connect failure on ready, a best-effort reconnect attempt never blocking markControllerReady", async () => {
    const state = newLegionState("omp", 1);
    state.controllerLocator = {
      tmuxSession: "legion-omp",
      tmuxWindowId: "@43",
      tmuxPaneId: "%1",
      socketPath: "/state/workers/controller.sock",
    };
    const { manager: processes } = manager(state, {
      connectWorkerRpc: async () => {
        throw new Error("ECONNREFUSED");
      },
    });

    await expect(processes.markControllerReady()).resolves.toBeUndefined();
  });

  it("restores runState to idle without firing onIdle when a queued idle-resume prompt rejects, keeping the assignment queued", async () => {
    const token = roleToken("omp", root, "tester");
    const client = fakeWorkerRpcClient();
    client.setRunStateSilently("idle");
    client.prompt = async (message: string) => {
      client.prompts.push(message);
      const previous = client.runState;
      client.setRunStateSilently("running");
      // Mirrors the real worker-rpc.ts fix: an ordinary rejection restores runState silently —
      // an undo, not a transition — never firing onIdle (see WorkerRpcClient.prompt's doc
      // comment). A "transition" restore here would re-trigger promoteWorkerQueue synchronously
      // mid-rejection: an unbounded retry storm for a persistently-broken client.
      client.setRunStateSilently(previous);
      throw new Error("shim write failed");
    };
    const { processes, state, managedState } = await workerCapFixture(2, {
      connectWorkerRpc: async () => client,
    });
    state.roles[token] = {
      issue: root,
      role: "tester",
      generation: 1,
      sessionId: "ses_tester",
      readyConfirmedAt: Date.parse("2026-08-24T00:00:00.000Z"),
      pendingAssignment: "verify #41",
      locator: {
        tmuxSession: "legion-omp",
        tmuxWindowId: "@42",
        tmuxPaneId: "%7",
        socketPath: "/state/workers/tester.sock",
      },
    };
    state.workerAdmission.queue.push(token);
    await processes.reconnectWorkers();

    await processes.reconcileWorkerAdmission();

    expect(client.runState).toBe("idle");
    expect(client.idleFireCount).toBe(0);
    expect(managedState.workerAdmission.queue).toEqual([token]);
    const claim = managedState.roles[token];
    if (!claim || !("issue" in claim)) throw new Error("tester claim disappeared");
    expect(claim.pendingAssignment).toBe("verify #41");
    expect(claim.locator).toBeDefined();
    expect(claim.promptFailures).toBe(1);
  });

  it("retires a persistently-rejecting queued worker after MAX_LAUNCH_FAILURES consecutive prompt rejections, falling through to a fresh launch on the next drain", async () => {
    const token = roleToken("omp", root, "tester");
    const client = fakeWorkerRpcClient();
    client.setRunStateSilently("idle");
    client.prompt = async (message: string) => {
      client.prompts.push(message);
      const previous = client.runState;
      client.setRunStateSilently("running");
      client.setRunStateSilently(previous);
      throw new Error("shim write failed");
    };
    const { processes, state, managedState } = await workerCapFixture(1, {
      connectWorkerRpc: async () => client,
      run: async (command) => {
        if (command[0] === "tmux" && command[1] === "split-window") {
          return { stdout: "%301 23456\n", exitCode: 0 };
        }
        if (
          command[0] === "tmux" &&
          command[1] === "list-panes" &&
          command.includes("#{pane_id}")
        ) {
          return { stdout: "%301\n", exitCode: 0 };
        }
        return { stdout: "", exitCode: 0 };
      },
    });
    state.roles[token] = {
      issue: root,
      role: "tester",
      generation: 1,
      sessionId: "ses_tester",
      readyConfirmedAt: Date.parse("2026-08-24T00:00:00.000Z"),
      pendingAssignment: "verify #41",
      locator: {
        tmuxSession: "legion-omp",
        tmuxWindowId: "@42",
        tmuxPaneId: "%7",
        socketPath: "/state/workers/tester.sock",
      },
    };
    state.workerAdmission.queue.push(token);
    await processes.reconnectWorkers();

    // Three separate drain attempts, each a rejected prompt — the third crosses
    // MAX_LAUNCH_FAILURES (3) and retires the pane (markWorkerDeadLocked, invoked via
    // WorkerAdmissionDeps.retireDeadClaim).
    await processes.reconcileWorkerAdmission();
    await processes.reconcileWorkerAdmission();
    await processes.reconcileWorkerAdmission();

    const retiredClaim = managedState.roles[token];
    if (!retiredClaim || !("issue" in retiredClaim)) throw new Error("tester claim disappeared");
    expect(retiredClaim.promptFailures).toBe(3);
    expect(retiredClaim.locator).toBeUndefined();
    expect(managedState.workerAdmission.queue).toEqual([token]);
    expect(client.prompts).toEqual(["verify #41", "verify #41", "verify #41"]);

    // Falls through to the launch path on the next drain: a fresh cold launch, not another
    // prompt attempt against the now-retired client.
    await processes.reconcileWorkerAdmission();

    expect(managedState.workerAdmission.queue).toEqual([]);
    const launchedClaim = managedState.roles[token];
    if (!launchedClaim || !("issue" in launchedClaim)) throw new Error("tester claim disappeared");
    expect(launchedClaim.locator).toBeDefined();
    expect(launchedClaim.locator?.tmuxPaneId).toBe("%301");
    expect(client.prompts).toEqual(["verify #41", "verify #41", "verify #41"]);
  });

  it("resets promptFailures to 0 on a successful prompt after prior rejections", async () => {
    const token = roleToken("omp", root, "tester");
    const client = fakeWorkerRpcClient();
    client.setRunStateSilently("idle");
    let shouldReject = true;
    client.prompt = async (message: string) => {
      client.prompts.push(message);
      if (shouldReject) {
        const previous = client.runState;
        client.setRunStateSilently("running");
        client.setRunStateSilently(previous);
        throw new Error("shim write failed");
      }
      client.emitRunState("running");
    };
    const { processes, state, managedState } = await workerCapFixture(2, {
      connectWorkerRpc: async () => client,
    });
    state.roles[token] = {
      issue: root,
      role: "tester",
      generation: 1,
      sessionId: "ses_tester",
      readyConfirmedAt: Date.parse("2026-08-24T00:00:00.000Z"),
      pendingAssignment: "verify #41",
      locator: {
        tmuxSession: "legion-omp",
        tmuxWindowId: "@42",
        tmuxPaneId: "%7",
        socketPath: "/state/workers/tester.sock",
      },
    };
    state.workerAdmission.queue.push(token);
    await processes.reconnectWorkers();

    await processes.reconcileWorkerAdmission();
    const rejectedClaim = managedState.roles[token];
    if (!rejectedClaim || !("issue" in rejectedClaim)) throw new Error("tester claim disappeared");
    expect(rejectedClaim.promptFailures).toBe(1);

    shouldReject = false;
    await processes.reconcileWorkerAdmission();

    expect(managedState.workerAdmission.queue).toEqual([]);
    const promotedClaim = managedState.roles[token];
    if (!promotedClaim || !("issue" in promotedClaim)) throw new Error("tester claim disappeared");
    expect(promotedClaim.promptFailures).toBe(0);
  });

  it("does not drop a queued idle-resume assignment as stale when its client is alive but not currently idle, only stops the drain until it goes idle", async () => {
    const token = roleToken("omp", root, "tester");
    const client = fakeWorkerRpcClient();
    const { processes, state, managedState } = await workerCapFixture(2, {
      connectWorkerRpc: async () => client,
    });
    state.roles[token] = {
      issue: root,
      role: "tester",
      generation: 1,
      sessionId: "ses_tester",
      readyConfirmedAt: Date.parse("2026-08-24T00:00:00.000Z"),
      pendingAssignment: "verify #41",
      locator: {
        tmuxSession: "legion-omp",
        tmuxWindowId: "@42",
        tmuxPaneId: "%7",
        socketPath: "/state/workers/tester.sock",
      },
    };
    state.workerAdmission.queue.push(token);
    await processes.reconnectWorkers();
    // The client is alive (a real session, confirmed sessionId) but currently mid-turn/busy —
    // not idle, so this queued idle-resume assignment must stay queued and untouched instead of
    // being dropped as stale (only a genuinely dead/never-connected client is stale here).
    client.emitRunState("running");

    await processes.reconcileWorkerAdmission();

    expect(managedState.workerAdmission.queue).toEqual([token]);
    const claim = managedState.roles[token];
    if (!claim || !("issue" in claim)) throw new Error("tester claim disappeared");
    expect(claim.pendingAssignment).toBe("verify #41");
    expect(claim.locator).toBeDefined();
    expect(client.prompts).toEqual([]);

    // Once it genuinely goes idle, the exact same queued assignment promotes normally.
    client.emitRunState("idle");
    await Bun.sleep(0);

    expect(managedState.workerAdmission.queue).toEqual([]);
    expect(client.prompts).toEqual(["verify #41"]);
  });

  it("leaves a queued idle-resume entry alone when its claim has a session but no ready confirmation, deferring to the ready path/watchdog", async () => {
    const token = roleToken("omp", root, "tester");
    const client = fakeWorkerRpcClient();
    client.setRunStateSilently("idle");
    const { processes, state, managedState } = await workerCapFixture(1, {
      connectWorkerRpc: async () => client,
    });
    state.roles[token] = {
      issue: root,
      role: "tester",
      generation: 1,
      sessionId: "ses_tester",
      pendingAssignment: "verify #41",
      // readyConfirmedAt deliberately absent: a restart landed between /worker/ready's ack and
      // its durable confirmation write, so this claim is neither stale (a real session and a
      // live client) nor safely promotable (admission cannot yet trust its readiness).
      locator: {
        tmuxSession: "legion-omp",
        tmuxWindowId: "@42",
        tmuxPaneId: "%7",
        socketPath: "/state/workers/tester.sock",
      },
    };
    state.workerAdmission.queue.push(token);
    await processes.reconnectWorkers();

    await processes.reconcileWorkerAdmission();

    expect(client.prompts).toEqual([]);
    expect(managedState.workerAdmission.queue).toEqual([token]);
    const claim = managedState.roles[token];
    if (!claim || !("issue" in claim)) throw new Error("tester claim disappeared");
    expect(claim.locator).toBeDefined();
    expect(claim.pendingAssignment).toBe("verify #41");
  });

  it("stops the drain after one below-threshold failure each for two queued tokens instead of burning every MAX_LAUNCH_FAILURES attempt on both in one pass", async () => {
    const failingTokenA = roleToken("omp", root, "planner");
    const failingTokenB = roleToken("omp", root, "tester");
    const { processes, state, managedState, stateDir } = await workerCapFixture(1);
    state.roles[failingTokenA] = {
      issue: root,
      role: "planner",
      pendingAssignment: "plan #41",
      // Deterministic, permanent failures (missing session files, never appear on retry) — but
      // launchFailures starts at 0 for both, so one attempt each stays *below* MAX_LAUNCH_FAILURES.
      resumeSessionFile: path.join(stateDir, "missing-planner-session.json"),
    };
    state.roles[failingTokenB] = {
      issue: root,
      role: "tester",
      pendingAssignment: "verify #41",
      resumeSessionFile: path.join(stateDir, "missing-tester-session.json"),
    };
    state.workerAdmission.queue.push(failingTokenA, failingTokenB);

    await processes.reconcileWorkerAdmission();

    // Without the attempted-token tracking, rotating a below-threshold failure to the tail
    // exposes the OTHER token as the new head, and rotating THAT one exposes the first again —
    // the drain would cycle both tokens through every MAX_LAUNCH_FAILURES attempt in this one
    // synchronous pass. With it: each token gets exactly one attempt this pass, both still
    // queued (rotated back to their original order — two rotations of a two-item queue is a
    // no-op on ordering), and the drain stops instead of continuing a third time.
    expect(managedState.workerAdmission.queue).toEqual([failingTokenA, failingTokenB]);
    const claimA = managedState.roles[failingTokenA];
    if (!claimA || !("issue" in claimA)) throw new Error("planner claim missing");
    expect(claimA.launchFailures).toBe(1);
    expect(claimA.locator).toBeUndefined();
    const claimB = managedState.roles[failingTokenB];
    if (!claimB || !("issue" in claimB)) throw new Error("tester claim missing");
    expect(claimB.launchFailures).toBe(1);
    expect(claimB.locator).toBeUndefined();
  });

  it("treats a saveState failure after a successful launch as a persistence issue, not a launch failure: keeps the locator and pendingAssignment, never bumps launchFailures, never re-queues", async () => {
    const token = roleToken("omp", root, "planner");
    const secondToken = roleToken("omp", root, "tester");
    let saveStateCalls = 0;
    const saveStateCalled = Promise.withResolvers<void>();
    const releaseSave = Promise.withResolvers<void>();
    const { processes, state, managedState } = await workerCapFixture(1, {
      saveState: async () => {
        saveStateCalls += 1;
        if (saveStateCalls === 1) {
          saveStateCalled.resolve();
          await releaseSave.promise;
          throw new Error("disk full");
        }
        // The one retry `launchWorker`'s own catch attempts also fails here, proving the
        // in-memory claim stays authoritative (never rethrown, never rotated, never retried a
        // second time) even when persistence never recovers within this call.
        throw new Error("disk still full");
      },
    });
    state.roles[token] = { issue: root, role: "planner", pendingAssignment: "plan #41" };
    state.roles[secondToken] = { issue: root, role: "tester", pendingAssignment: "verify #41" };
    state.workerAdmission.queue.push(token, secondToken);

    const reconciling = processes.reconcileWorkerAdmission();
    await saveStateCalled.promise;

    // Planner's failing save is still gated. `launchWorker` already spliced planner out of the
    // queue (in memory) before calling this gated save — proving the second queued token is
    // completely untouched while planner's own attempt is in flight: `drainWorkerQueue`'s loop
    // awaits each token's own `promoteQueuedWorker` call fully before peeking the next one.
    expect(managedState.workerAdmission.queue).toEqual([secondToken]);
    const untouchedTester = managedState.roles[secondToken];
    if (!untouchedTester || !("issue" in untouchedTester)) {
      throw new Error("tester claim missing");
    }
    expect(untouchedTester.locator).toBeUndefined();

    releaseSave.resolve();
    await reconciling;

    // The pane already exists at this point (a real, running worker-shim process) — a
    // `saveState` failure here is a durable-state persistence issue, not a launch failure:
    // `launchFailures` is never touched at all (a launch success carries forward whatever it
    // was before -- 0 for a claim with no prior failures, exactly as here -- only a durably
    // confirmed `/worker/ready` ever resets it), the token is never
    // re-queued (it already has a live pane; re-queuing it would launch a second pane for the
    // same issue/role the next time it is promoted), and the locator plus `pendingAssignment`
    // stay exactly as `launchWorker` wrote them — the in-memory claim remains authoritative
    // even though both persist attempts failed. The pane's own `/worker/started` ->
    // `/worker/ready` handshake calls back into the daemon independent of this save.
    expect(saveStateCalls).toBe(2);
    const claim = managedState.roles[token];
    if (!claim || !("issue" in claim)) throw new Error("planner claim missing");
    expect(claim.launchFailures).toBe(0);
    expect(claim.locator).toBeDefined();
    expect(claim.pendingAssignment).toBe("plan #41");
    // Planner's own (never-rolled-back) locator keeps cap 1 fully occupied, so the second
    // queued token correctly never promotes either.
    expect(managedState.workerAdmission.queue).toEqual([secondToken]);
  });

  it("boot reconciliation kills an unrecorded worker-shim pane split into a known window, leaves recorded panes (even in an otherwise-unknown window) alone", async () => {
    const stateDir = await temporaryDir();
    const state = newLegionState("omp", 1);
    tree(state);
    if (state.trees[root]?.locator) state.trees[root].locator.tmuxPaneId = "%42";
    const recordedPaneId = state.trees[root]?.locator?.tmuxPaneId;
    if (!recordedPaneId) throw new Error("test root tree is missing its own recorded pane id");
    const commands: string[][] = [];
    const { manager: processes } = manager(state, {
      config: config(stateDir),
      run: async (command) => {
        commands.push(command);
        if (command[0] === "tmux" && command[1] === "list-windows") {
          // The tree's own window is known (excluded from `known` in this fixture only via the
          // orphan-pane path below, not this one) — return no unowned windows, isolating this
          // test to pane-level reconciliation only.
          return { stdout: "", exitCode: 0 };
        }
        if (command[0] === "tmux" && command[1] === "list-panes" && command[2] === "-a") {
          return {
            stdout: [
              // Recorded: this tree's own root pane. Must never be reaped, even tagged with a
              // window id this fixture's `known` set (built from locators only) does not
              // separately special-case — `known.has(paneId)` alone must protect it.
              `${recordedPaneId}\t@42\tlegion-omp\tworker-shim --socket x -- omp --mode rpc\t1000`,
              // Unrecorded worker-shim pane split into the SAME known window @42 — exactly the
              // crash-window orphan item 3 targets: a real process the daemon forgot about.
              `%99\t@42\tlegion-omp\tworker-shim --socket y -- omp --mode rpc\t1000`,
              // A pane in a window owned by a different session entirely — never touched.
              `%50\t@50\tlegion-other\tworker-shim --socket z -- omp --mode rpc\t1000`,
            ].join("\n"),
            exitCode: 0,
          };
        }
        return { stdout: "", exitCode: 0 };
      },
    });

    await processes.reconcileTmuxWindows(0);

    const killedPanes = commands
      .filter((command) => command[0] === "tmux" && command[1] === "kill-pane")
      .map((command) => command[3]);
    expect(killedPanes).toEqual(["%99"]);
  });

  it("exempts a known window's panes from reaping when its owning tree/controller locator never recorded a pane id, while still reaping a genuine orphan pane in a fully-recorded known window", async () => {
    const stateDir = await temporaryDir();
    const state = newLegionState("omp", 1);
    tree(state);
    // `tree()`'s default now carries a `tmuxPaneId` (needed by the graceful-stop tests
    // elsewhere in this file) -- reconstructed without it here to restore the pre-backfill
    // state this exemption protects.
    const rootLocator = state.trees[root]?.locator;
    if (!rootLocator) throw new Error("test setup expects tree() to have recorded a root locator");
    const { tmuxPaneId: _rootPaneId, ...rootLocatorWithoutPaneId } = rootLocator;
    const rootTree = state.trees[root];
    if (!rootTree) throw new Error("test setup expects tree() to have recorded a root tree");
    rootTree.locator = rootLocatorWithoutPaneId;
    state.controllerLocator = {
      tmuxSession: "legion-omp",
      tmuxWindowId: "@43",
      socketPath: "/state/controller.sock",
    };
    const plannerToken = roleToken("omp", root, "planner");
    state.roles[plannerToken] = {
      issue: root,
      role: "planner",
      locator: {
        tmuxSession: "legion-omp",
        tmuxWindowId: "@44",
        tmuxPaneId: "%44",
        socketPath: "/state/workers/planner.sock",
      },
    };
    const commands: string[][] = [];
    const { manager: processes } = manager(state, {
      config: config(stateDir),
      run: async (command) => {
        commands.push(command);
        if (command[0] === "tmux" && command[1] === "list-windows") {
          // Every window here (@42, @43, @44) is known via a recorded `tmuxWindowId` — none are
          // candidates for window-level reaping regardless of whether their owner's pane id is
          // recorded; isolates this test to the pane-level pass.
          return { stdout: "", exitCode: 0 };
        }
        if (command[0] === "tmux" && command[1] === "list-panes" && command[2] === "-a") {
          return {
            stdout: [
              // Root tree's own pane, in its known window @42 — but this locator never recorded
              // a pane id, so this pane cannot be distinguished from a real orphan by id alone.
              // Must be exempted, not killed.
              `%10\t@42\tlegion-omp\tworker-shim --socket root -- omp --mode rpc\t1000`,
              // Controller's own pane, same situation, in its known window @43.
              `%11\t@43\tlegion-omp\tworker-shim --socket ctrl -- omp --mode rpc\t1000`,
              // Planner's own recorded pane in its fully-recorded known window @44 — never a
              // reap candidate; `known.has(paneId)` alone already protects it.
              `%44\t@44\tlegion-omp\tworker-shim --socket planner -- omp --mode rpc\t1000`,
              // A genuinely unrecorded worker-shim pane split into that SAME fully-recorded
              // window @44 — no ambiguity here (the window's owner's own pane id IS known), so
              // this one is still a real orphan and must be reaped exactly as before.
              `%99\t@44\tlegion-omp\tworker-shim --socket orphan -- omp --mode rpc\t1000`,
            ].join("\n"),
            exitCode: 0,
          };
        }
        return { stdout: "", exitCode: 0 };
      },
    });

    await processes.reconcileTmuxWindows(0);

    const killedPanes = commands
      .filter((command) => command[0] === "tmux" && command[1] === "kill-pane")
      .map((command) => command[3]);
    expect(killedPanes).toEqual(["%99"]);
  });

  it("backfills a tree locator's missing pane id once probe confirms it alive, and a controller locator's once controllerAlive confirms it alive", async () => {
    const stateDir = await temporaryDir();
    const state = newLegionState("omp", 1);
    tree(state);
    // `tree()`'s default now carries a `tmuxPaneId` (needed by the graceful-stop tests
    // elsewhere in this file) -- reconstructed without it here to restore the pre-backfill
    // state this test exercises (see the exemption test above for the same invariant asserted
    // directly).
    const rootLocator = state.trees[root]?.locator;
    if (!rootLocator) throw new Error("test setup expects tree() to have recorded a root locator");
    const { tmuxPaneId: _rootPaneId, ...rootLocatorWithoutPaneId } = rootLocator;
    const rootTree = state.trees[root];
    if (!rootTree) throw new Error("test setup expects tree() to have recorded a root tree");
    rootTree.locator = rootLocatorWithoutPaneId;
    state.controllerLocator = {
      tmuxSession: "legion-omp",
      tmuxWindowId: "@43",
      socketPath: "/state/controller.sock",
    };
    let saveStateCalls = 0;
    const { manager: processes } = manager(state, {
      config: config(stateDir),
      saveState: async () => {
        saveStateCalls += 1;
      },
      readProcessCmdline: async () => "omp\0",
      run: async (command) => {
        if (
          command[0] === "tmux" &&
          command[1] === "list-panes" &&
          command.includes("#{pane_id}")
        ) {
          const windowId = command[2] === "-t" ? command[3] : undefined;
          if (windowId === "@42") return { stdout: "%42\n", exitCode: 0 };
          if (windowId === "@43") return { stdout: "%43\n", exitCode: 0 };
          return { stdout: "", exitCode: 1 };
        }
        if (
          command[0] === "tmux" &&
          command[1] === "list-panes" &&
          command.includes("#{pane_pid}")
        ) {
          return { stdout: "4242\n", exitCode: 0 };
        }
        if (command[0] === "kill") return { stdout: "", exitCode: 0 };
        return { stdout: "", exitCode: 0 };
      },
    });

    expect(await processes.probe(root)).toBe("alive");
    expect(state.trees[root]?.locator?.tmuxPaneId).toBe("%42");

    await processes.ensureController();
    expect(state.controllerLocator?.tmuxPaneId).toBe("%43");
    expect(saveStateCalls).toBeGreaterThanOrEqual(2);
  });
});

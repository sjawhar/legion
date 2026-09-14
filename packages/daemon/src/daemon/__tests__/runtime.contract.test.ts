import { afterAll, describe, expect, it, spyOn } from "bun:test";
import { existsSync, readFileSync } from "node:fs";
import { mkdir, mkdtemp, readFile, rm, stat } from "node:fs/promises";
import os from "node:os";
import path from "node:path";
import { type IssueKey, type LegionRole, roleToken } from "@legion/contracts";
import { adoptWorkingCopyCommand } from "@legion/workspace";
import { DEFAULT_KUBERNETES_RESOURCES, DEFAULT_ROLE_PROFILES } from "../config";
import { parseImageDigestRef } from "../image-ref";
import { createK8sClient } from "../k8s-client";
import {
  awaitShutdown,
  boundedWait,
  DAEMON_CLI_ENTRYPOINT,
  type Locator,
  locatorHandles,
  ProcessStopFailed,
  probeWorker,
  type Runtime,
  type SpawnSpec,
  sameProcess,
} from "../runtime";
import { KubernetesRuntime } from "../runtime-kubernetes";
import { TmuxRuntime, type TmuxRuntimeDeps } from "../runtime-tmux";
import type { WorkerRpcClient } from "../worker-rpc";
import { procStatLine } from "./ci-fixtures";
import { createFakeK8sApi } from "./fake-k8s-api";
import { FakeRuntime, type FakeWorkerRpcClient, fakeWorkerRpcClient } from "./fake-runtime";

const issue: IssueKey = "LEGION-42";
const tempDirs: string[] = [];
afterAll(async () => {
  await Promise.all(tempDirs.map((dir) => rm(dir, { recursive: true, force: true })));
});

interface HarnessOptions {
  /** Every client the runtime hands out (or dials itself) has a `shutdown` that never closes
   * the socket, so a graceful stop can only end by timeout. */
  neverCloses?: boolean;
}

/** One `Runtime` under test plus the observation points the shared cases need. A runtime is
 * built fresh per case; `sleep` is always the no-wait fake so a timeout race settles at once. */
interface Harness {
  runtime: Runtime;
  /** The `runtime` discriminant every locator this harness spawns must carry. */
  expectedRuntime: Locator["runtime"];
  makeSpec(role: LegionRole | "controller", forIssue?: IssueKey): SpawnSpec;
  /** How many raw socket dials the runtime itself has made so far. */
  dials(): number;
  /** Every client the runtime created — via `connect` or its own stop-time dial. */
  clients(): readonly ShutdownCountingClient[];
  /** The handle `locator` names now belongs to some other process (tmux: its pane id reissued
   * to a new process; the fake: the process moved out of the alive table into `strangers`). */
  occupy(locator: Locator): Promise<void>;
}

/** The start ticks `FakeTmuxServer` gives the process with `pid` (see `newPane`). */
const startTicksOf = (pid: number): number => 1_000_000 + pid * 7;

type ShutdownCountingClient = FakeWorkerRpcClient & { shutdowns: number };

function countingClient(neverCloses: boolean): ShutdownCountingClient {
  const client = fakeWorkerRpcClient() as ShutdownCountingClient;
  client.shutdowns = 0;
  const realShutdown = client.shutdown.bind(client);
  client.shutdown = () => {
    client.shutdowns += 1;
    if (!neverCloses) realShutdown();
  };
  return client;
}

function makeSpec(role: LegionRole | "controller", forIssue?: IssueKey): SpawnSpec {
  return {
    ...(role === "controller"
      ? {}
      : { issue: forIssue ?? issue, tree: forIssue ?? issue, generation: 1 }),
    role,
    env: { LEGION_ROLE: role, UNSET: undefined },
    launch:
      role === "controller"
        ? { promptPath: "/roles/controller-root.md" }
        : { promptPath: `/roles/${role}.md`, addressingPrompt: `address ${role}` },
    secrets:
      role === "controller"
        ? { LEGION_CONTROLLER_SECRET: "controller-secret" }
        : { LEGION_BOOT_TOKEN: "boot-token" },
  };
}

/** The tmux harness's provisioning runner: records nothing and answers every `jj`/`git` command
 * with exit 0, creating `<dir>/.jj` for `jj git clone <remote> <dir>` and `<dir>` for
 * `jj workspace add <dir>` — the two side effects `provisionIssueWorkspace` checks for on disk. */
async function provisioningRun(
  command: string[]
): Promise<{ stdout: string; stderr: string; exitCode: number }> {
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
  return { stdout: "", stderr: "", exitCode: 0 };
}

async function fakeHarness(options: HarnessOptions = {}): Promise<Harness> {
  const clients: ShutdownCountingClient[] = [];
  const runtime = new FakeRuntime({
    clientFactory: () => {
      const client = countingClient(options.neverCloses ?? false);
      clients.push(client);
      return client;
    },
    sleep: async () => {},
  });
  return {
    runtime,
    expectedRuntime: "kubernetes",
    makeSpec,
    dials: () => runtime.connects.length,
    clients: () => clients,
    occupy: async (locator) => runtime.occupyHandle(locator),
  };
}

interface FakePane {
  pid: number;
  /** What `/proc/<pid>/stat` field 22 reports for this pane's process. */
  startTicks: number;
  startCommand: string;
  activityAt: number;
}

interface FakeWindow {
  owner?: string;
  activityAt: number;
  panes: Map<string, FakePane>;
}

/** An in-memory private tmux server answering exactly the argv shapes `tmux.ts` builds. */
class FakeTmuxServer {
  readonly commands: string[][] = [];
  readonly windows = new Map<string, FakeWindow>();
  sessionExists = false;
  /** A `new-session` is in flight (parked on `newSessionGate`): real tmux refuses a second one
   * with `duplicate session` while the first is still forking the server. */
  sessionCreating = false;
  /** Holds the next `new-session` open until resolved — the fork time a real server takes, and
   * the window in which a second first-ever spawn can arrive. */
  newSessionGate: PromiseWithResolvers<void> | undefined;
  /** Resolves when the first `new-session` is issued (before it parks on `newSessionGate`): the
   * event a race test awaits instead of polling, since a spawn's provisioning does real fs writes
   * before its first tmux command. */
  readonly newSessionIssued = Promise.withResolvers<void>();
  /** Overrides the next `new-session`'s result (exit code and stderr) when set; the session is
   * not created. */
  newSessionResult: { exitCode: number; stderr?: string } | undefined;
  /** Overrides the next `new-window` result; when it tears the server down, later commands see
   * the session absent just as they would after tmux reaps its last window. */
  newWindowResult: { exitCode: number; stderr?: string; endsSession?: boolean } | undefined;
  /** Ordered `new-window` failures for recovery tests. */
  readonly newWindowResults: Array<{
    exitCode: number;
    stderr?: string;
    endsSession?: boolean;
  }> = [];
  /** Resolves when a `new-window` is issued, before its configured failure returns. */
  readonly newWindowIssued = Promise.withResolvers<void>();
  /** Overrides the next `kill-pane`'s result (exit code and stderr) when set. */
  killPaneResult: { exitCode: number; stderr?: string } | undefined;
  /** Overrides the next per-pane `list-panes -t <pane>` result (exit code and stderr) when set. */
  listPanesResult: { exitCode: number; stderr?: string } | undefined;
  private nextWindow = 42;
  private nextPane = 1;
  private nextPid = 12345;

  constructor(private readonly session: string) {}

  /** A window this server already had (a human's, or a previous daemon's): the sweep's raw
   * material. Panes get sequential ids and pids like a spawned one would. */
  addWindow(windowId: string, owner: string | undefined, paneCount: number): string[] {
    const panes = new Map<string, FakePane>();
    const ids: string[] = [];
    for (let i = 0; i < paneCount; i += 1) {
      const paneId = `%${this.nextPane}`;
      this.nextPane += 1;
      panes.set(paneId, this.newPane("legion worker-shim --socket x -- omp"));
      ids.push(paneId);
    }
    this.windows.set(windowId, { owner, activityAt: 0, panes });
    return ids;
  }

  windowOf(paneId: string): string | undefined {
    for (const [windowId, window] of this.windows) {
      if (window.panes.has(paneId)) return windowId;
    }
    return undefined;
  }

  pane(paneId: string): FakePane {
    const windowId = this.windowOf(paneId);
    const pane = windowId === undefined ? undefined : this.windows.get(windowId)?.panes.get(paneId);
    if (!pane) throw new Error(`fake tmux: no pane ${paneId}`);
    return pane;
  }

  private pidOwner(pid: number): FakePane | undefined {
    for (const window of this.windows.values()) {
      for (const pane of window.panes.values()) if (pane.pid === pid) return pane;
    }
    return undefined;
  }

  /** `/proc/<pid>/stat` for a live pane's process: field 22 is that pane's `startTicks`. Rejects
   * (ENOENT) for a pid no pane runs, exactly like the real file once the process is gone. */
  async procStat(pid: number): Promise<string> {
    const pane = this.pidOwner(pid);
    if (!pane) {
      throw Object.assign(
        new Error(`ENOENT: no such file or directory, open '/proc/${pid}/stat'`),
        {
          code: "ENOENT",
        }
      );
    }
    return procStatLine(pid, pane.startTicks, "legion worker-shim");
  }

  /** The process in `paneId` exits and tmux hands the very same pane id to a new process (a
   * server recreate, or a `respawn-pane`): a fresh pid and start ticks behind an unchanged id.
   * Returns the new process's identity so a test can tell the two apart. */
  reissuePane(paneId: string): { pid: number; startTicks: number } {
    const pane = this.pane(paneId);
    const fresh = this.newPane(pane.startCommand);
    pane.pid = fresh.pid;
    pane.startTicks = fresh.startTicks;
    return { pid: pane.pid, startTicks: pane.startTicks };
  }

  private newPane(startCommand: string): FakePane {
    const pid = this.nextPid;
    this.nextPid += 1;
    // Start ticks are derived from the pid so every process in a run has a distinct, stable
    // identity a test can predict; nothing about the check depends on the relation.
    return { pid, startTicks: 1_000_000 + pid * 7, startCommand, activityAt: 0 };
  }

  run = async (
    command: string[]
  ): Promise<{ stdout: string; stderr?: string; exitCode: number }> => {
    this.commands.push(command);
    if (command[0] !== "tmux") return { stdout: "", exitCode: 0 };
    const verb = command[3];
    const target = command[command.indexOf("-t") + 1];
    switch (verb) {
      case "has-session":
        return { stdout: "", exitCode: this.sessionExists ? 0 : 1 };
      case "new-session": {
        if (this.sessionExists || this.sessionCreating) {
          return { stdout: "", stderr: `duplicate session: ${this.session}`, exitCode: 1 };
        }
        this.sessionCreating = true;
        this.newSessionIssued.resolve();
        await this.newSessionGate?.promise;
        this.sessionCreating = false;
        if (this.newSessionResult) {
          const result = this.newSessionResult;
          this.newSessionResult = undefined;
          return { stdout: "", ...result };
        }
        this.sessionExists = true;
        return { stdout: "", exitCode: 0 };
      }
      case "set-option":
        if (command.includes("-w")) {
          const window = this.windows.get(target);
          if (!window) return { stdout: "no such window", exitCode: 1 };
          window.owner = command[command.length - 1];
        }
        return { stdout: "", exitCode: 0 };
      case "new-window": {
        this.newWindowIssued.resolve();
        const nextResult = this.newWindowResults.shift() ?? this.newWindowResult;
        if (nextResult) {
          this.newWindowResult = undefined;
          const { endsSession, ...result } = nextResult;
          if (endsSession) {
            this.sessionExists = false;
            this.windows.clear();
          }
          return { stdout: "", ...result };
        }
        if (!this.sessionExists) {
          return {
            stdout: "",
            stderr: "no server running on /tmp/tmux-1000/legion-omp",
            exitCode: 1,
          };
        }
        const windowId = `@${this.nextWindow}`;
        this.nextWindow += 1;
        const paneId = `%${this.nextPane}`;
        this.nextPane += 1;
        const pane = this.newPane(command[command.length - 1] ?? "");
        this.windows.set(windowId, { activityAt: 0, panes: new Map([[paneId, pane]]) });
        return { stdout: `${windowId} ${paneId} ${pane.pid}\n`, exitCode: 0 };
      }
      case "split-window": {
        const window = this.windows.get(target);
        if (!window) return { stdout: "can't find window", exitCode: 1 };
        const paneId = `%${this.nextPane}`;
        this.nextPane += 1;
        window.panes.set(paneId, this.newPane(command[command.length - 1] ?? ""));
        return { stdout: `${paneId} ${window.panes.get(paneId)?.pid}\n`, exitCode: 0 };
      }
      case "select-layout":
        return { stdout: "", exitCode: 0 };
      case "list-panes": {
        if (command.includes("-a")) {
          const rows: string[] = [];
          for (const [windowId, window] of this.windows) {
            for (const [paneId, pane] of window.panes) {
              rows.push(
                `${paneId}\t${windowId}\t${window.owner ?? ""}\t${pane.startCommand}\t${pane.activityAt / 1000}`
              );
            }
          }
          return { stdout: `${rows.join("\n")}\n`, exitCode: 0 };
        }
        if (this.listPanesResult) {
          const result = this.listPanesResult;
          this.listPanesResult = undefined;
          return { stdout: "", ...result };
        }
        const windowId = target.startsWith("%") ? this.windowOf(target) : target;
        const window = windowId === undefined ? undefined : this.windows.get(windowId);
        if (!window) return { stdout: "", stderr: "can't find pane", exitCode: 1 };
        const format = command[command.indexOf("-F") + 1];
        const rows = [...window.panes].map(([paneId, pane]) =>
          format === "#{pane_id} #{pane_pid}" ? `${paneId} ${pane.pid}` : paneId
        );
        return { stdout: `${rows.join("\n")}\n`, exitCode: 0 };
      }
      case "list-windows": {
        if (!this.sessionExists) return { stdout: "", exitCode: 1 };
        const rows = [...this.windows].map(
          ([windowId, window]) => `${windowId}\t${window.owner ?? ""}\t${window.activityAt / 1000}`
        );
        return { stdout: `${rows.join("\n")}\n`, exitCode: 0 };
      }
      case "kill-window":
        if (target === `${this.session}:__legion_bootstrap`) return { stdout: "", exitCode: 0 };
        return { stdout: "", exitCode: this.windows.delete(target) ? 0 : 1 };
      case "kill-pane": {
        if (this.killPaneResult) {
          const result = this.killPaneResult;
          this.killPaneResult = undefined;
          return { stdout: "", ...result };
        }
        const windowId = this.windowOf(target);
        if (windowId === undefined) return { stdout: "", stderr: "can't find pane", exitCode: 1 };
        const window = this.windows.get(windowId);
        window?.panes.delete(target);
        if (window && window.panes.size === 0) this.windows.delete(windowId);
        return { stdout: "", exitCode: 0 };
      }
      default:
        throw new Error(`fake tmux: unexpected verb ${verb}`);
    }
  };
}

interface TmuxHarness extends Harness {
  server: FakeTmuxServer;
  stateDir: string;
  /** Every locator `spawn` returned, in order — what `issueLocators` hands the runtime. A test
   * that swaps one for a copy (as `/process/started` does) re-keys it with `registerLocator`. */
  locators: Locator[];
  registerLocator: (locator: Locator, issue: IssueKey) => void;
  socketPaths: string[];
  /** The `timeoutMs` each dial was given. */
  timeouts: Array<number | undefined>;
  cmdlineReads: number[];
  /** Every pid whose `/proc/<pid>/stat` the runtime read, in order. */
  statReads: number[];
}

async function tmuxHarness(
  options: HarnessOptions & {
    /** Every dial is refused, as against a shim that is already gone. */
    connectFails?: boolean;
    /** A graceful stop's timeout never fires, so only a real close can end the wait. */
    hangSleep?: boolean;
    readProcessCmdline?: (pid: number) => Promise<string>;
    readProcessStat?: (pid: number) => Promise<string>;
    now?: () => number;
    /** Observes (and may hold) every provisioning command; defaults to `provisioningRun`. */
    provisioningRun?: TmuxRuntimeDeps["run"];
    /** Observes each provisioning's token request — the first thing `provisionIssueWorkspace`
     * does, before any command or fs work — so a test can tell when a spawn's provisioning was
     * admitted. Defaults to a constant token. */
    provisioningToken?: TmuxRuntimeDeps["provisioningToken"];
    /** Signals after a spawn has provisioned its workspace and is about to enter the per-issue
     * lane, so a race test can wait for the second root without a timing budget. */
    onIssueLookup?: (issue: IssueKey) => void;
  } = {}
): Promise<TmuxHarness> {
  const stateDir = await mkdtemp(path.join(os.tmpdir(), "legion-runtime-tmux-"));
  tempDirs.push(stateDir);
  const server = new FakeTmuxServer("legion-omp");
  const clients: ShutdownCountingClient[] = [];
  const socketPaths: string[] = [];
  const timeouts: Array<number | undefined> = [];
  const locators: Locator[] = [];
  const cmdlineReads: number[] = [];
  const statReads: number[] = [];
  const issueOf = new Map<Locator, IssueKey>();
  const runtime = new TmuxRuntime({
    tmux: { run: server.run, socket: "legion-omp" },
    project: "omp",
    stateDir,
    ompInvocation: "omp",
    ompLaunchPrefix: [],
    statPrompt: async () => {},
    provisioningToken: options.provisioningToken ?? (async () => "token"),
    run: options.provisioningRun ?? provisioningRun,
    repo: "acme/widgets",
    credentialHelper: "!legion credential",
    slowCommandTimeoutMs: 1000,
    connectWorkerRpc: async (socketPath, timeoutMs) => {
      socketPaths.push(socketPath);
      timeouts.push(timeoutMs);
      if (options.connectFails) throw new Error("ECONNREFUSED");
      const client = countingClient(options.neverCloses ?? false);
      clients.push(client);
      return client;
    },
    workerRpcTimeoutMs: () => 5_000,
    now: options.now ?? (() => 0),
    sleep: options.hangSleep ? () => new Promise<void>(() => {}) : async () => {},
    readProcessCmdline: async (pid) => {
      cmdlineReads.push(pid);
      return options.readProcessCmdline ? options.readProcessCmdline(pid) : "omp\0";
    },
    readProcessStat: async (pid) => {
      statReads.push(pid);
      return options.readProcessStat ? options.readProcessStat(pid) : server.procStat(pid);
    },
    issueLocators: (forIssue) => {
      options.onIssueLookup?.(forIssue);
      return locators.filter(
        (locator) => locator.runtime === "tmux" && issueOf.get(locator) === forIssue
      );
    },
  });
  // Records every spawned locator by issue so `issueLocators` sees what state would.
  const spawningRuntime: Runtime = {
    launchesController: runtime.launchesController,
    removesWorkspacesOnTreeClose: runtime.removesWorkspacesOnTreeClose,
    spawn: async (kind, spec) => {
      const locator = await runtime.spawn(kind, spec);
      if (spec.issue) {
        issueOf.set(locator, spec.issue);
        locators.push(locator);
      }
      return locator;
    },
    adoptWorkingCopy: (issue, role, identity, timeoutMs) =>
      runtime.adoptWorkingCopy(issue, role, identity, timeoutMs),
    probe: (locator) => runtime.probe(locator),
    connect: (locator, timeoutMs) => runtime.connect(locator, timeoutMs),
    stop: (locator, timeoutMs, stopOptions) => runtime.stop(locator, timeoutMs, stopOptions),
    reconcileOrphans: (known, graceMs) => runtime.reconcileOrphans(known, graceMs),
  };
  return {
    runtime: spawningRuntime,
    expectedRuntime: "tmux",
    makeSpec,
    dials: () => socketPaths.length,
    clients: () => clients,
    occupy: async (locator) => {
      if (locator.runtime !== "tmux" || !locator.tmuxPaneId) throw new Error("tmux locator");
      server.reissuePane(locator.tmuxPaneId);
    },
    server,
    stateDir,
    locators,
    registerLocator: (locator, issue) => {
      issueOf.set(locator, issue);
    },
    socketPaths,
    timeouts,
    cmdlineReads,
    statReads,
  };
}

/** `KubernetesRuntime` over the in-memory fake API server. The pod's shim is simulated: the
 * moment `spawn` returns, a counting client is registered under the locator's claim token, as the
 * listener would on the shim's hello -- the runtime itself never dials. */
async function kubernetesHarness(options: HarnessOptions = {}): Promise<Harness> {
  const clock = Date.parse("2026-09-13T00:00:00.000Z");
  const api = createFakeK8sApi({ namespace: "legion", now: () => clock });
  const clients: ShutdownCountingClient[] = [];
  const registrations = new Map<string, WorkerRpcClient>();
  let awaits = 0;
  const listener = {
    registrations,
    awaitRegistration: async (token: string, _timeoutMs: number) => {
      awaits += 1;
      const client = registrations.get(token);
      if (!client) throw new Error(`no stream for ${token}`);
      return client;
    },
  };
  const runtime = new KubernetesRuntime({
    project: "omp",
    config: {
      namespace: "legion",
      image: parseImageDigestRef(`ghcr.io/x/y@sha256:${"a".repeat(64)}`),
      treeVolume: "20Gi",
      resources: DEFAULT_KUBERNETES_RESOURCES,
      roleProfiles: DEFAULT_ROLE_PROFILES,
    },
    client: createK8sClient({ server: "https://fake", namespace: "legion", fetch: api.fetch }),
    listener: () => listener,
    repo: "acme/widgets",
    provisioningToken: async () => "installation-token",
    daemonUrl: "http://172.18.0.1:19370",
    workerStreamPort: 19371,
    workerBootTimeoutMs: 120_000,
    workerBootRegistrationDeadlineIntervals: 3,
    workerStopTimeoutMs: 10_000,
    workerRpcTimeoutMs: () => 5_000,
    readFile: async (file) => `text of ${file}`,
    now: () => clock,
    sleep: async () => {},
  });
  const spawning: Runtime = {
    launchesController: runtime.launchesController,
    removesWorkspacesOnTreeClose: runtime.removesWorkspacesOnTreeClose,
    spawn: async (kind, spec) => {
      const locator = await runtime.spawn(kind, spec);
      if (locator.runtime !== "kubernetes") throw new Error("kubernetes locator");
      const client = countingClient(options.neverCloses ?? false);
      clients.push(client);
      registrations.set(locator.roleToken, client);
      const forget = () => registrations.delete(locator.roleToken);
      void client.closed.then(forget, forget);
      return locator;
    },
    adoptWorkingCopy: (issue, role, identity, timeoutMs) =>
      runtime.adoptWorkingCopy(issue, role, identity, timeoutMs),
    probe: (locator) => runtime.probe(locator),
    connect: (locator, timeoutMs) => runtime.connect(locator, timeoutMs),
    stop: (locator, timeoutMs, stopOptions) => runtime.stop(locator, timeoutMs, stopOptions),
    reconcileOrphans: (known, graceMs) => runtime.reconcileOrphans(known, graceMs),
  };
  return {
    runtime: spawning,
    expectedRuntime: "kubernetes",
    makeSpec,
    dials: () => awaits,
    clients: () => clients,
    occupy: async (locator) => {
      if (locator.runtime !== "kubernetes") throw new Error("kubernetes locator");
      api.reissueUid(locator.podName);
    },
  };
}

const harnesses: Array<[string, (options?: HarnessOptions) => Promise<Harness>]> = [
  ["FakeRuntime", fakeHarness],
  ["TmuxRuntime", tmuxHarness],
  ["KubernetesRuntime", kubernetesHarness],
];

describe.each(harnesses)("Runtime contract: %s", (_name, makeHarness) => {
  it("spawn returns a locator of this runtime that probe reports alive", async () => {
    const harness = await makeHarness();
    const locator = await harness.runtime.spawn("worker", harness.makeSpec("implementer"));
    expect(locator.runtime).toBe(harness.expectedRuntime);
    expect((await harness.runtime.probe(locator)).status).toBe("alive");
  });

  it("connect returns a client whose negotiate resolves", async () => {
    const harness = await makeHarness();
    const locator = await harness.runtime.spawn("root", harness.makeSpec("architect"));
    const client = await harness.runtime.connect(locator, 50);
    await expect(client.negotiate()).resolves.toBeUndefined();
  });

  it("stop with skipGraceful never dials and leaves the process dead", async () => {
    const harness = await makeHarness();
    const locator = await harness.runtime.spawn("worker", harness.makeSpec("tester"));
    await harness.runtime.stop(locator, 50, { skipGraceful: true });
    expect(harness.dials()).toBe(0);
    expect((await harness.runtime.probe(locator)).status).toBe("dead");
  });

  it("probe distinguishes a process that is gone from a handle another process now occupies, and stop never destroys that other process", async () => {
    const harness = await makeHarness();
    const gone = await harness.runtime.spawn("worker", harness.makeSpec("planner"));
    await harness.runtime.stop(gone, 50, { skipGraceful: true });
    expect(await harness.runtime.probe(gone)).toEqual({ status: "dead", reason: "gone" });

    const taken = await harness.runtime.spawn("worker", harness.makeSpec("tester"));
    await harness.occupy(taken);
    const verdict = await harness.runtime.probe(taken);
    expect(verdict).toEqual({
      status: "dead",
      reason: "not-recorded-process",
      detail: expect.any(String),
    });
    const consoleError = spyOn(console, "error").mockImplementation(() => {});
    try {
      await harness.runtime.stop(taken, 50, { skipGraceful: true });
      await harness.runtime.stop(taken, 50, { skipGraceful: true, refuseKill: true });
    } finally {
      consoleError.mockRestore();
    }
    // Whatever now holds the handle is still there: neither stop destroyed it.
    expect(await harness.runtime.probe(taken)).toEqual(verdict);
  });

  it("stop with refuseKill asks the process to exit but never destroys it", async () => {
    const harness = await makeHarness({ neverCloses: true });
    const locator = await harness.runtime.spawn("worker", harness.makeSpec("reviewer"));
    await harness.runtime.connect(locator, 50);
    await harness.runtime.stop(locator, 50, { refuseKill: true });
    const shutdowns = harness.clients().reduce((sum, client) => sum + client.shutdowns, 0);
    expect(shutdowns).toBe(1);
    expect((await harness.runtime.probe(locator)).status).toBe("alive");
  });

  it("graceful stop sends exactly one shutdown frame and still ends dead when closed never settles", async () => {
    const harness = await makeHarness({ neverCloses: true });
    const locator = await harness.runtime.spawn("worker", harness.makeSpec("reviewer"));
    await harness.runtime.connect(locator, 50);
    await harness.runtime.stop(locator, 50);
    const shutdowns = harness.clients().reduce((sum, client) => sum + client.shutdowns, 0);
    expect(shutdowns).toBe(1);
    expect((await harness.runtime.probe(locator)).status).toBe("dead");
  });

  it("reconcileOrphans keeps a process whose handles are known and reaps one whose are not", async () => {
    const harness = await makeHarness();
    const locator = await harness.runtime.spawn("worker", harness.makeSpec("planner"));
    await harness.runtime.reconcileOrphans(new Set(locatorHandles(locator)), 0);
    expect((await harness.runtime.probe(locator)).status).toBe("alive");
    await harness.runtime.reconcileOrphans(new Set(), 0);
    expect((await harness.runtime.probe(locator)).status).toBe("dead");
  });

  it("sameProcess identifies a locator with itself and never with another spawn", async () => {
    const harness = await makeHarness();
    const first = await harness.runtime.spawn("worker", harness.makeSpec("implementer"));
    const second = await harness.runtime.spawn("worker", harness.makeSpec("tester"));
    expect(sameProcess(first, first)).toBe(true);
    expect(sameProcess(first, { ...first })).toBe(true);
    expect(sameProcess(first, second)).toBe(false);
    expect(sameProcess(first, undefined)).toBe(false);
    expect(sameProcess(undefined, undefined)).toBe(true);
  });
});

describe("TmuxRuntime", () => {
  const tmuxArgv = (...rest: string[]): string[] => ["tmux", "-L", "legion-omp", ...rest];
  /** The one tmux round trip `verifyPaneProcess` makes for a pane that verifies: the watched
   * pane's own pid from its window listing. Existence and identity come from the `/proc` reads
   * that follow. */
  const verifyArgv = (paneId: string): string[][] => [
    tmuxArgv("list-panes", "-t", paneId, "-F", "#{pane_id} #{pane_pid}"),
  ];
  const socketFor = (stateDir: string, name: string): string =>
    path.join(stateDir, "workers", `${name}.sock`);
  const shimCommand = (workspaceDir: string, socketPath: string, innerCommand: string): string =>
    `cd ${workspaceDir} && ${process.execPath} ${DAEMON_CLI_ENTRYPOINT} worker-shim --socket ${socketPath} -- ${innerCommand}`;
  /** Where `provisionIssueWorkspace` puts the issue's working copy under the harness's state
   * dir (`<stateDir>/workspaces/<owner>/<repo>/<issue-lower>`): the pane's `cd` target. */
  const workspaceFor = (stateDir: string): string =>
    path.join(stateDir, "workspaces", "acme", "widgets", "legion-42");
  /** The inner OMP command the runtime assembles from `makeSpec(role)`'s launch description: the
   * invocation, RPC mode, and the one double-quoted `--append-system-prompt` word --
   * `$(cat <prompt>)`, then the addressing text, separated by a blank line (`systemPromptArguments`'
   * format; OMP's flag is last-wins, so one argument carries every fragment; the controller has
   * no addressing fragment). */
  const ompCommand = (role: LegionRole | "controller"): string =>
    role === "controller"
      ? `omp --mode rpc --append-system-prompt "$(cat /roles/controller-root.md)"`
      : `omp --mode rpc --append-system-prompt "$(cat /roles/${role}.md)\n\naddress ${role}"`;

  it("sameProcess treats a reissued pane -- same id, other pid or start ticks -- as a different process", async () => {
    const harness = await tmuxHarness();
    const locator = await harness.runtime.spawn("worker", harness.makeSpec("tester"));
    if (locator.runtime !== "tmux") throw new Error("tmux locator");
    expect(sameProcess(locator, { ...locator, panePid: (locator.panePid ?? 0) + 1 })).toBe(false);
    expect(
      sameProcess(locator, { ...locator, paneStartTicks: (locator.paneStartTicks ?? 0) + 1 })
    ).toBe(false);
    // Two legacy records without identity compare by pane id alone.
    const { panePid: _pid, paneStartTicks: _ticks, ...legacy } = locator;
    expect(sameProcess(legacy, { ...legacy })).toBe(true);
    expect(sameProcess(legacy, locator)).toBe(false);
  });

  it("spawns a root into a fresh window with the exact tmux argv, the boot token in a 0600 file, and its _FILE pointer as the last -e pair", async () => {
    const harness = await tmuxHarness();
    const locator = await harness.runtime.spawn("root", harness.makeSpec("architect"));
    const bootTokenFile = path.join(
      harness.stateDir,
      "secrets",
      roleToken("omp", issue, "architect")
    );
    const socketPath = socketFor(harness.stateDir, "architect-9e2fb104");
    expect(harness.server.commands).toEqual([
      tmuxArgv("has-session", "-t", "legion-omp"),
      // One invocation (`;` = tmux's command separator): the session exists with an empty
      // `update-environment`, so no attach can ever copy a client's SSH_AUTH_SOCK/SSH_CONNECTION/
      // DISPLAY into the session table every later pane inherits — not even one landing between
      // creation and the option.
      tmuxArgv(
        "new-session",
        "-d",
        "-s",
        "legion-omp",
        "-n",
        "__legion_bootstrap",
        "sleep 3600",
        ";",
        "set-option",
        "-t",
        "legion-omp",
        "update-environment",
        ""
      ),
      tmuxArgv("set-option", "-t", "legion-omp", "@legion_owner", "legion-omp"),
      tmuxArgv(
        "new-window",
        "-P",
        "-F",
        "#{window_id} #{pane_id} #{pane_pid}",
        "-t",
        "legion-omp",
        "-n",
        "legion-42",
        "-e",
        "LEGION_ROLE=architect",
        "-e",
        `LEGION_ROOT_WORKSPACE=${workspaceFor(harness.stateDir)}`,
        "-e",
        `LEGION_BOOT_TOKEN_FILE=${bootTokenFile}`,
        shimCommand(workspaceFor(harness.stateDir), socketPath, ompCommand("architect"))
      ),
      tmuxArgv("kill-window", "-t", "legion-omp:__legion_bootstrap"),
      tmuxArgv("set-option", "-w", "-t", "@42", "@legion_owner", "legion-omp"),
    ]);
    expect(locator).toEqual({
      runtime: "tmux",
      tmuxSession: "legion-omp",
      tmuxWindowId: "@42",
      tmuxPaneId: "%1",
      socketPath,
      panePid: 12345,
      paneStartTicks: startTicksOf(12345),
    });
    expect(await readFile(bootTokenFile, "utf8")).toBe("boot-token");
    expect((await stat(bootTokenFile)).mode & 0o777).toBe(0o600);
    expect((await stat(path.dirname(bootTokenFile))).mode & 0o777).toBe(0o700);
  });

  it("splits a second process for the same issue into its recorded window once that window's pane verifies", async () => {
    const harness = await tmuxHarness();
    await harness.runtime.spawn("root", harness.makeSpec("architect"));
    harness.server.commands.length = 0;
    const worker = await harness.runtime.spawn("worker", harness.makeSpec("implementer"));
    const socketPath = socketFor(harness.stateDir, "implementer-9e2fb104");
    expect(harness.server.commands).toEqual([
      ...verifyArgv("%1"),
      tmuxArgv(
        "split-window",
        "-t",
        "@42",
        "-P",
        "-F",
        "#{pane_id} #{pane_pid}",
        "-e",
        "LEGION_ROLE=implementer",
        "-e",
        `LEGION_WORKSPACE=${workspaceFor(harness.stateDir)}`,
        "-e",
        `LEGION_BOOT_TOKEN_FILE=${path.join(harness.stateDir, "secrets", roleToken("omp", issue, "implementer"))}`,
        shimCommand(workspaceFor(harness.stateDir), socketPath, ompCommand("implementer"))
      ),
      tmuxArgv("select-layout", "-t", "@42", "tiled"),
    ]);
    // The root's identity at its launch, re-read to verify its pane before the split, then the
    // new pane's own at its launch.
    expect(harness.statReads).toEqual([12345, 12345, 12346]);
    expect(worker).toEqual({
      runtime: "tmux",
      tmuxSession: "legion-omp",
      tmuxWindowId: "@42",
      tmuxPaneId: "%2",
      socketPath,
      panePid: 12346,
      paneStartTicks: startTicksOf(12346),
    });
  });

  it("delivers spec.env.PATH through the pane shell command, never as a -e pair tmux would discard (LEGION-91)", async () => {
    const harness = await tmuxHarness();
    const panePath = `${path.join(harness.stateDir, "worker-bin")}:/full/bin:/usr/bin`;
    await harness.runtime.spawn("worker", {
      ...harness.makeSpec("tester"),
      env: { LEGION_ROLE: "tester", PATH: panePath, UNSET: undefined },
    });
    const launch = harness.server.commands.find((cmd) => cmd[3] === "new-window");
    if (!launch) throw new Error("no new-window");
    // Every other variable still rides -e; PATH rides none.
    expect(launch).toContain("LEGION_ROLE=tester");
    expect(launch.some((part) => part.startsWith("PATH="))).toBe(false);
    // The pane shell exports it before anything else runs, so worker-shim and OMP inherit it.
    expect(launch.at(-1)).toBe(
      `export PATH=${panePath} && ${shimCommand(workspaceFor(harness.stateDir), socketFor(harness.stateDir, "tester-9e2fb104"), ompCommand("tester"))}`
    );
  });

  it("single-quotes a PATH the shell would otherwise split", async () => {
    const harness = await tmuxHarness();
    await harness.runtime.spawn("worker", {
      ...harness.makeSpec("tester"),
      env: { PATH: "/state dir/worker-bin:/usr/bin" },
    });
    const launch = harness.server.commands.find((cmd) => cmd[3] === "new-window");
    expect(
      launch
        ?.at(-1)
        ?.startsWith(
          "export PATH='/state dir/worker-bin:/usr/bin' && cd " +
            `${workspaceFor(harness.stateDir)} && `
        )
    ).toBe(true);
  });

  it("opens a fresh window when no recorded pane verifies, leaves every recorded locator's window alone, and splits later spawns into the fresh one", async () => {
    const harness = await tmuxHarness();
    const root = await harness.runtime.spawn("root", harness.makeSpec("architect"));
    const first = await harness.runtime.spawn("worker", harness.makeSpec("planner"));
    expect(first.runtime === "tmux" && first.tmuxWindowId).toBe("@42");
    // A human killed the issue's window out from under the daemon.
    harness.server.windows.delete("@42");
    const next = await harness.runtime.spawn("worker", harness.makeSpec("implementer"));
    expect(next.runtime === "tmux" && next.tmuxWindowId).toBe("@43");
    expect(harness.server.commands.filter((c) => c[3] === "new-window")).toHaveLength(2);
    // A locator's window id is a fact about where its pane lived; it is never rewritten.
    for (const locator of [root, first]) {
      expect(locator.runtime === "tmux" && locator.tmuxWindowId).toBe("@42");
    }
    // A fourth spawn verifies the fresh window's pane and splits into it rather than opening a
    // third, even though every persisted locator still names the dead window.
    const another = await harness.runtime.spawn("worker", harness.makeSpec("tester"));
    expect(another.runtime === "tmux" && another.tmuxWindowId).toBe("@43");
    expect(harness.server.commands.filter((c) => c[3] === "new-window")).toHaveLength(2);
  });

  it("never splits into a recorded window whose pane now runs another process, even when the window is live", async () => {
    const harness = await tmuxHarness();
    const root = await harness.runtime.spawn("root", harness.makeSpec("architect"));
    if (root.runtime !== "tmux" || !root.tmuxPaneId) throw new Error("tmux locator");
    // The window is still there; its only pane now belongs to some other process.
    harness.server.reissuePane(root.tmuxPaneId);
    const worker = await harness.runtime.spawn("worker", harness.makeSpec("implementer"));
    expect(worker.runtime === "tmux" && worker.tmuxWindowId).toBe("@43");
    expect(harness.server.commands.filter((c) => c[3] === "split-window")).toHaveLength(0);
    expect(root.tmuxWindowId).toBe("@42");
  });

  it("verifies a recorded pane once even when state holds a copy of the launched locator, not the object spawn returned", async () => {
    const harness = await tmuxHarness();
    const root = await harness.runtime.spawn("root", harness.makeSpec("architect"));
    if (root.runtime !== "tmux" || !root.tmuxPaneId) throw new Error("tmux locator");
    // `/process/started` and `/worker/started` replace the stored locator with a spread copy:
    // same pane, same identity, a different object. Make state look exactly like that.
    const index = harness.locators.indexOf(root);
    expect(index).toBeGreaterThanOrEqual(0);
    const copy: Locator = { ...root };
    harness.locators.splice(index, 1, copy);
    // (The harness keys issues by locator object; re-key the copy so `issueLocators` yields it.)
    const issue = harness.makeSpec("architect").issue;
    if (!issue) throw new Error("spec without issue");
    harness.registerLocator(copy, issue);
    // The pane no longer runs the recorded process, so every recorded window fails to verify --
    // the path that walks the whole list. The copy and the in-memory window entry name the same
    // pane, so it is listed once, not twice.
    harness.server.reissuePane(root.tmuxPaneId);
    harness.server.commands.length = 0;
    const worker = await harness.runtime.spawn("worker", harness.makeSpec("implementer"));
    expect(worker.runtime === "tmux" && worker.tmuxWindowId).toBe("@43");
    expect(
      harness.server.commands.filter((c) => c[3] === "list-panes" && c[5] === root.tmuxPaneId)
    ).toHaveLength(1);
  });

  it("a concurrent second spawn on an issue splits into the window the first one is opening", async () => {
    const harness = await tmuxHarness();
    const [root, worker] = await Promise.all([
      harness.runtime.spawn("root", harness.makeSpec("architect")),
      harness.runtime.spawn("worker", harness.makeSpec("planner")),
    ]);
    expect(root.runtime === "tmux" && root.tmuxWindowId).toBe("@42");
    expect(worker.runtime === "tmux" && worker.tmuxWindowId).toBe("@42");
    expect(harness.server.commands.filter((c) => c[3] === "new-window")).toHaveLength(1);
    // The fresh window's pane was fully recorded -- and verified -- before the second spawn
    // decided to split into it.
    const newWindowAt = harness.server.commands.findIndex((c) => c[3] === "new-window");
    const splitAt = harness.server.commands.findIndex((c) => c[3] === "split-window");
    const verifiedAt = harness.server.commands.findIndex(
      (c, index) => index > newWindowAt && c[3] === "list-panes" && c[5] === "%1"
    );
    expect(newWindowAt).toBeLessThan(verifiedAt);
    expect(verifiedAt).toBeLessThan(splitAt);
  });

  /** One real macrotask yield (`setImmediate`, never a wall-clock delay): the one step a spawn
   * that has reached the session step (see `onIssueLookup`) needs to park on the lane — or,
   * without the lane, to run its own `has-session` and `new-session`. */
  const onceEventLoop = (): Promise<void> => {
    const { promise, resolve } = Promise.withResolvers<void>();
    setImmediate(resolve);
    return promise;
  };
  const verb = (command: string[]): string | undefined => command[3];
  const windowName = (command: string[]): string | undefined => command[command.indexOf("-n") + 1];
  const isBootstrapKill = (command: string[]): boolean =>
    verb(command) === "kill-window" && command[5] === "legion-omp:__legion_bootstrap";

  /** A `tmuxHarness` whose `onIssueLookup` resolves a per-issue promise (`reachedIssue`), so a
   * test can tell that a second spawn has reached the session step (see the hook's docblock). */
  const racingHarness = async () => {
    const reached = new Map<IssueKey, PromiseWithResolvers<void>>();
    const entryFor = (forIssue: IssueKey): PromiseWithResolvers<void> => {
      let entry = reached.get(forIssue);
      if (!entry) {
        entry = Promise.withResolvers<void>();
        reached.set(forIssue, entry);
      }
      return entry;
    };
    const harness = await tmuxHarness({
      onIssueLookup: (forIssue) => entryFor(forIssue).resolve(),
    });
    return { ...harness, reachedIssue: (forIssue: IssueKey) => entryFor(forIssue).promise };
  };

  it("two concurrent first spawns on different issues share one new-session; only the creator kills the bootstrap window, once", async () => {
    const harness = await racingHarness();
    const { server } = harness;
    server.newSessionGate = Promise.withResolvers<void>();
    const first = harness.runtime.spawn("root", harness.makeSpec("architect", "LEGION-42")).then(
      (value) => ({ status: "fulfilled" as const, value }),
      (reason) => ({ status: "rejected" as const, reason })
    );
    await server.newSessionIssued.promise;
    const second = harness.runtime.spawn("root", harness.makeSpec("architect", "LEGION-43")).then(
      (value) => ({ status: "fulfilled" as const, value }),
      (reason) => ({ status: "rejected" as const, reason })
    );
    await harness.reachedIssue("LEGION-43");
    await onceEventLoop();
    server.newSessionGate.resolve();
    const [firstOutcome, secondOutcome] = await Promise.all([first, second]);
    expect([firstOutcome.status, secondOutcome.status]).toEqual(["fulfilled", "fulfilled"]);
    if (firstOutcome.status !== "fulfilled" || secondOutcome.status !== "fulfilled") {
      throw new Error("tmux session creation did not open both windows");
    }
    const [a, b] = [firstOutcome.value, secondOutcome.value];
    const verbs = server.commands.map(verb);
    expect(verbs.filter((value) => value === "has-session")).toHaveLength(1);
    expect(verbs.filter((value) => value === "new-session")).toHaveLength(1);
    const newWindows = server.commands.filter((command) => verb(command) === "new-window");
    expect(newWindows.map(windowName).sort()).toEqual(["legion-42", "legion-43"]);
    expect(server.commands.filter(isBootstrapKill)).toHaveLength(1);
    const creatorWindowAt = server.commands.findIndex(
      (command) => verb(command) === "new-window" && windowName(command) === "legion-42"
    );
    expect(server.commands.findIndex(isBootstrapKill)).toBeGreaterThan(creatorWindowAt);

    if (a.runtime !== "tmux" || b.runtime !== "tmux") throw new Error("tmux locators");
    expect(a.tmuxWindowId).not.toBe(b.tmuxWindowId);
    for (const locator of [a, b]) {
      if (!locator.tmuxWindowId) throw new Error("window id");
      expect(server.windows.get(locator.tmuxWindowId)?.owner).toBe("legion-omp");
    }
  });

  it("a root arriving while the controller is creating the session waits for it and opens its own window; the controller alone kills the bootstrap window", async () => {
    const harness = await racingHarness();
    const { server } = harness;
    server.newSessionGate = Promise.withResolvers<void>();
    const controller = harness.runtime.spawn("controller", harness.makeSpec("controller")).then(
      (value) => ({ status: "fulfilled" as const, value }),
      (reason) => ({ status: "rejected" as const, reason })
    );
    await server.newSessionIssued.promise;
    const root = harness.runtime.spawn("root", harness.makeSpec("architect")).then(
      (value) => ({ status: "fulfilled" as const, value }),
      (reason) => ({ status: "rejected" as const, reason })
    );
    await harness.reachedIssue(issue);
    await onceEventLoop();
    server.newSessionGate.resolve();
    const [controllerOutcome, rootOutcome] = await Promise.all([controller, root]);
    expect([controllerOutcome.status, rootOutcome.status]).toEqual(["fulfilled", "fulfilled"]);
    if (controllerOutcome.status !== "fulfilled" || rootOutcome.status !== "fulfilled") {
      throw new Error("tmux session creation did not open both windows");
    }
    const [controllerLocator, rootLocator] = [controllerOutcome.value, rootOutcome.value];

    const verbs = server.commands.map(verb);
    expect(verbs.filter((value) => value === "has-session")).toHaveLength(1);
    expect(verbs.filter((value) => value === "new-session")).toHaveLength(1);
    const newWindows = server.commands.filter((command) => verb(command) === "new-window");
    expect(newWindows.map(windowName)).toEqual(["controller", "legion-42"]);
    expect(server.commands.filter(isBootstrapKill)).toHaveLength(1);
    const controllerWindowAt = server.commands.findIndex(
      (command) => verb(command) === "new-window" && windowName(command) === "controller"
    );
    expect(server.commands.findIndex(isBootstrapKill)).toBeGreaterThan(controllerWindowAt);
    if (controllerLocator.runtime !== "tmux" || rootLocator.runtime !== "tmux") {
      throw new Error("tmux locators");
    }
    expect(controllerLocator.tmuxWindowId).not.toBe(rootLocator.tmuxWindowId);
  });

  it("a new-session that fails for a real reason fails the creator and every waiter with tmux's stderr, attempts no new-window, and the next spawn starts over from has-session", async () => {
    const harness = await racingHarness();
    const { server } = harness;
    server.newSessionGate = Promise.withResolvers<void>();
    server.newSessionResult = {
      exitCode: 1,
      stderr: "error creating /tmp/tmux-1000/legion-omp (Permission denied)",
    };
    const first = harness.runtime.spawn("root", harness.makeSpec("architect", "LEGION-42"));
    await server.newSessionIssued.promise;
    const second = harness.runtime.spawn("root", harness.makeSpec("architect", "LEGION-43"));
    await harness.reachedIssue("LEGION-43");
    await onceEventLoop();
    server.newSessionGate.resolve();

    const message =
      "tmux new-session failed (exit 1): error creating /tmp/tmux-1000/legion-omp (Permission denied)";
    const settled = await Promise.allSettled([first, second]);
    expect(
      settled.map((outcome) => outcome.status === "rejected" && (outcome.reason as Error).message)
    ).toEqual([message, message]);
    const verbs = server.commands.map(verb);
    expect(verbs.filter((value) => value === "has-session")).toHaveLength(1);
    expect(verbs.filter((value) => value === "new-session")).toHaveLength(1);
    expect(verbs.filter((value) => value === "new-window")).toHaveLength(0);
    expect(verbs.filter((value) => value === "kill-window")).toHaveLength(0);
    expect(verbs.filter((value) => value === "set-option")).toHaveLength(0);

    server.commands.length = 0;
    const retried = await harness.runtime.spawn("root", harness.makeSpec("architect", "LEGION-43"));
    expect(retried.runtime === "tmux" && retried.tmuxWindowId).toBe("@42");
    expect(server.commands.slice(0, 2).map(verb)).toEqual(["has-session", "new-session"]);
    expect(server.commands.filter(isBootstrapKill)).toHaveLength(1);
  });

  it("serializes recovery after a new-window failure with a concurrent first spawn, so both roots open windows after one recreation", async () => {
    const harness = await racingHarness();
    const { server } = harness;
    server.sessionExists = true;
    server.newSessionGate = Promise.withResolvers<void>();
    server.newWindowResult = {
      exitCode: 1,
      stderr: "no server running on /tmp/tmux-1000/legion-omp",
      endsSession: true,
    };

    const first = harness.runtime.spawn("root", harness.makeSpec("architect", "LEGION-42")).then(
      (value) => ({ status: "fulfilled" as const, value }),
      (reason) => ({ status: "rejected" as const, reason })
    );
    await server.newWindowIssued.promise;
    const second = harness.runtime.spawn("root", harness.makeSpec("architect", "LEGION-43")).then(
      (value) => ({ status: "fulfilled" as const, value }),
      (reason) => ({ status: "rejected" as const, reason })
    );
    await harness.reachedIssue("LEGION-43");
    await onceEventLoop();
    server.newSessionGate.resolve();
    const [firstOutcome, secondOutcome] = await Promise.all([first, second]);

    expect([firstOutcome.status, secondOutcome.status]).toEqual(["fulfilled", "fulfilled"]);
    if (firstOutcome.status !== "fulfilled" || secondOutcome.status !== "fulfilled") {
      throw new Error("tmux session recovery did not open both windows");
    }
    const verbs = server.commands.map(verb);
    expect(verbs.filter((value) => value === "has-session")).toHaveLength(2);
    expect(verbs.filter((value) => value === "new-session")).toHaveLength(1);
    expect(verbs.filter((value) => value === "new-window")).toHaveLength(3);
    expect(server.commands.filter(isBootstrapKill)).toHaveLength(1);
    expect(firstOutcome.value.runtime === "tmux" && firstOutcome.value.tmuxWindowId).toBeDefined();
    expect(
      secondOutcome.value.runtime === "tmux" && secondOutcome.value.tmuxWindowId
    ).toBeDefined();
  });

  it("names both window failures when the replacement session's first window fails", async () => {
    const harness = await tmuxHarness();
    const { server } = harness;
    server.sessionExists = true;
    server.newWindowResults.push(
      {
        exitCode: 1,
        stderr: "no server running on /tmp/tmux-1000/legion-omp",
        endsSession: true,
      },
      { exitCode: 1, stderr: "Y" }
    );

    let message = "";
    await harness.runtime.spawn("root", harness.makeSpec("architect")).catch((error: Error) => {
      message = error.message;
    });

    expect(message.startsWith("tmux new-window failed (exit 1): Y")).toBe(true);
    expect(message).toContain("on the second attempt");
    expect(message).toContain("after has-session reported legion-omp present and then gone");
    expect(message).toContain("first attempt: tmux new-window failed (exit 1): no server running");
    expect(server.commands.filter((command) => verb(command) === "new-window")).toHaveLength(2);
    expect(server.commands.filter((command) => verb(command) === "new-session")).toHaveLength(1);
  });

  it("serializes workspace provisioning per repository: a second tree's clone and git-config writes wait for the first's to finish", async () => {
    // Two trees admitted in one sweep share the repository clone; git's `.git/config` lock refuses
    // a concurrent writer (`could not lock config file … File exists`). The first spawn's
    // provisioning is held at its first command while the second spawn is started. A spawn's
    // provisioning is admitted when it asks for its token -- `provisionIssueWorkspace`'s first
    // act, before any command or fs work -- so the token requests witness admission: with the
    // first held, the second must not have been admitted; once released, the second is admitted
    // only after the first's last command, and the recorded commands form two contiguous blocks
    // -- every command of the first tree (through its last `git config` write) before the second
    // tree's opening `jj git fetch`. Nothing here waits on the clock.
    const gate = Promise.withResolvers<void>();
    const firstCommand = Promise.withResolvers<void>();
    const commands: string[][] = [];
    /** `commands.length` at each provisioning's admission. */
    const admittedAt: number[] = [];
    const harness = await tmuxHarness({
      provisioningToken: async () => {
        admittedAt.push(commands.length);
        return "token";
      },
      provisioningRun: async (command) => {
        commands.push(command);
        if (commands.length === 1) {
          firstCommand.resolve();
          await gate.promise;
        }
        return provisioningRun(command);
      },
    });
    const first = harness.runtime.spawn("worker", harness.makeSpec("planner", "LEGION-42"));
    await firstCommand.promise;
    const second = harness.runtime.spawn("worker", harness.makeSpec("planner", "LEGION-43"));
    // Let everything already runnable run (the second spawn's own promise reactions; no timed
    // wait): with the first still held, only one provisioning has been admitted.
    await new Promise<void>((resolve) => setImmediate(resolve));
    expect(admittedAt).toEqual([0]);
    gate.resolve();
    const [one, two] = await Promise.all([first, second]);
    expect(one.runtime).toBe("tmux");
    expect(two.runtime).toBe("tmux");
    // A tree's provisioning opens with `jj git clone` (no shared clone yet) or, with the clone
    // present, the `jj config get git.abandon-unreachable-commits` read that precedes its fetch;
    // the read that immediately follows a tree's own clone is not an opener.
    const isSettingsRead = (c: string[] | undefined) =>
      c?.[0] === "jj" && c[1] === "config" && c[2] === "get";
    const openers = commands.flatMap((c, i) =>
      (c[0] === "jj" && c[1] === "git" && c[2] === "clone") ||
      (isSettingsRead(c) &&
        !(i > 0 && commands[i - 1]?.[1] === "git" && commands[i - 1]?.[2] === "clone"))
        ? [i]
        : []
    );
    expect(openers).toHaveLength(2);
    // The second tree was admitted exactly when the first's block had finished.
    expect(admittedAt).toEqual([0, openers[1]]);
    const [firstTree, secondTree] = [
      commands.slice(openers[0], openers[1]),
      commands.slice(openers[1]),
    ];
    const workspaceAdded = (block: string[][]) =>
      block.filter((c) => c[0] === "jj" && c[1] === "workspace" && c[2] === "add").map((c) => c[3]);
    expect(workspaceAdded(firstTree)).toEqual([
      path.join(harness.stateDir, "workspaces", "acme", "widgets", "legion-42"),
    ]);
    expect(workspaceAdded(secondTree)).toEqual([
      path.join(harness.stateDir, "workspaces", "acme", "widgets", "legion-43"),
    ]);
    // `git worktree prune` plus the five `git config` writes, each tree's, never interleaved.
    expect(firstTree.filter((c) => c[0] === "git")).toHaveLength(6);
    expect(secondTree.filter((c) => c[0] === "git")).toHaveLength(6);
  });

  it("adopts the working copy for the assigned role on a live-idle re-prompt: the shared jj metaedit command runs on the daemon-host workspace under the role's identity, and its failure is the adoption's", async () => {
    // An implementer left idle after its phase is re-prompted for a new assignment once the
    // tester has handed off; every role shares the issue's one working copy, so its undescribed
    // `@` must be re-authored for the role that is about to describe it.
    const commands: Array<{ command: string[]; env?: NodeJS.ProcessEnv; timeoutMs?: number }> = [];
    let exitCode = 0;
    const harness = await tmuxHarness({
      provisioningRun: async (command, options) => {
        if (command[1] === "metaedit") {
          commands.push({ command, env: options?.env, timeoutMs: options?.timeoutMs });
          return { stdout: "", stderr: "Error: no such revision", exitCode };
        }
        return provisioningRun(command);
      },
    });
    await harness.runtime.spawn("worker", harness.makeSpec("implementer"));
    const identity = {
      jjUser: "legion-implementer[bot]",
      jjEmail: "implementer@users.noreply.github.com",
    };
    await harness.runtime.adoptWorkingCopy(issue, "implementer", identity, 300_000);
    expect(commands).toEqual([
      {
        command: adoptWorkingCopyCommand(
          path.join(harness.stateDir, "workspaces", "acme", "widgets", "legion-42")
        ),
        env: { JJ_USER: identity.jjUser, JJ_EMAIL: identity.jjEmail },
        timeoutMs: 300_000,
      },
    ]);
    exitCode = 1;
    await expect(
      harness.runtime.adoptWorkingCopy(issue, "implementer", identity, 300_000)
    ).rejects.toThrow(
      "Could not adopt LEGION-42's working copy for implementer: Command failed (exit 1): jj metaedit"
    );
  });

  it("spawns the controller into its own window with the controller socket and secret file", async () => {
    const harness = await tmuxHarness();
    const locator = await harness.runtime.spawn("controller", harness.makeSpec("controller"));
    const secretFile = path.join(harness.stateDir, "secrets", "legion-omp-controller");
    const socketPath = socketFor(harness.stateDir, "controller");
    const window = harness.server.commands.find((c) => c[3] === "new-window");
    expect(window).toEqual(
      tmuxArgv(
        "new-window",
        "-P",
        "-F",
        "#{window_id} #{pane_id} #{pane_pid}",
        "-t",
        "legion-omp",
        "-n",
        "controller",
        "-e",
        "LEGION_ROLE=controller",
        "-e",
        `LEGION_CONTROLLER_SECRET_FILE=${secretFile}`,
        shimCommand(path.join(harness.stateDir, "controller"), socketPath, ompCommand("controller"))
      )
    );
    expect(locator).toEqual({
      runtime: "tmux",
      tmuxSession: "legion-omp",
      tmuxWindowId: "@42",
      tmuxPaneId: "%1",
      socketPath,
      panePid: 12345,
      paneStartTicks: startTicksOf(12345),
    });
    expect(await readFile(secretFile, "utf8")).toBe("controller-secret");
    expect((await stat(secretFile)).mode & 0o777).toBe(0o600);
  });

  it("creates the controller directory without an OMP project config", async () => {
    const harness = await tmuxHarness();

    await harness.runtime.spawn("controller", harness.makeSpec("controller"));

    const controllerDir = path.join(harness.stateDir, "controller");
    expect(existsSync(controllerDir)).toBeTrue();
    expect(existsSync(path.join(controllerDir, ".omp", "config.yml"))).toBeFalse();
  });

  it("refuses a spec that does not fit one tmux process", async () => {
    const harness = await tmuxHarness();
    await expect(
      harness.runtime.spawn("worker", {
        ...harness.makeSpec("tester"),
        secrets: { LEGION_BOOT_TOKEN: "a", EXTRA: "b" },
      })
    ).rejects.toThrow(/exactly one secret/);
    const { issue: _issue, ...withoutIssue } = harness.makeSpec("tester");
    await expect(harness.runtime.spawn("worker", withoutIssue)).rejects.toThrow(
      "spawn worker requires spec.issue"
    );
    await expect(
      harness.runtime.spawn("root", { ...harness.makeSpec("architect"), role: "controller" })
    ).rejects.toThrow(/requires a Legion role/);
    expect(harness.server.commands).toEqual([]);
  });

  it("probe never trusts a locator without a recorded identity, and never asks tmux about it", async () => {
    const harness = await tmuxHarness();
    harness.server.sessionExists = true;
    harness.server.addWindow("@42", "legion-omp", 1);
    const withPane: Locator = {
      runtime: "tmux",
      tmuxSession: "legion-omp",
      tmuxWindowId: "@42",
      tmuxPaneId: "%1",
    };
    expect(await harness.runtime.probe(withPane)).toEqual({
      status: "dead",
      reason: "not-recorded-process",
      detail: "pane %1 has no recorded process identity (locator predates identity tracking)",
    });
    const paneless: Locator = { runtime: "tmux", tmuxSession: "legion-omp", tmuxWindowId: "@42" };
    expect(await harness.runtime.probe(paneless)).toEqual({
      status: "dead",
      reason: "not-recorded-process",
      detail: "pane @42 has no recorded process identity (locator predates identity tracking)",
    });
    expect(harness.server.commands).toEqual([]);
    expect(harness.statReads).toEqual([]);
  });

  it("probe checks pid, then start ticks, then OMP -- against the watched pane's own row, never a sibling's", async () => {
    const harness = await tmuxHarness({
      // Only the architect's pane (the window's first) still runs OMP.
      readProcessCmdline: async (pid) => (pid === 12345 ? "omp\0" : "sleep\0"),
    });
    harness.server.sessionExists = true;
    const [, worker] = harness.server.addWindow("@1464", "legion-omp", 3);
    if (!worker) throw new Error("fixture: three panes");
    const recorded = harness.server.pane(worker);
    const locator: Locator = {
      runtime: "tmux",
      tmuxSession: "legion-omp",
      tmuxWindowId: "@1464",
      tmuxPaneId: worker,
      panePid: recorded.pid,
      paneStartTicks: recorded.startTicks,
    };
    // Pid and start ticks match the sibling's own row (never the architect's), so the read chain
    // runs to the OMP check and fails there: one stat read and one cmdline read, both for the
    // worker's pid. The detail carries both identities; the reads are the contract.
    expect(await harness.runtime.probe(locator)).toEqual({
      status: "dead",
      reason: "not-recorded-process",
      detail: expect.stringMatching(/pid 12346 .*recorded pid 12346 start/),
    });
    expect(harness.statReads).toEqual([12346]);
    expect(harness.cmdlineReads).toEqual([12346]);
  });

  it("probe reports a pane whose process is not the recorded one with both identities, and a gone pane as gone", async () => {
    const harness = await tmuxHarness();
    const locator = await harness.runtime.spawn("worker", harness.makeSpec("tester"));
    if (locator.runtime !== "tmux" || !locator.tmuxPaneId) throw new Error("tmux locator");
    expect(await harness.runtime.probe(locator)).toEqual({ status: "alive", pid: 12345 });

    // The same pane id, a new process behind it: pid differs. The detail carries both identities
    // (what the pane reports now, what the locator recorded); the sentence around them is not
    // the contract -- the reads `statReads`/`cmdlineReads` record are.
    const reissued = harness.server.reissuePane(locator.tmuxPaneId);
    const recorded = `recorded pid 12345 start ${locator.paneStartTicks}`;
    const bothIdentities = (observed: string) =>
      expect.stringMatching(new RegExp(`${observed}.*${recorded}`));
    expect(await harness.runtime.probe(locator)).toEqual({
      status: "dead",
      reason: "not-recorded-process",
      detail: bothIdentities(`pid ${reissued.pid}`),
    });
    // No stat read past a pid mismatch: the two so far are the launch's and the alive probe's.
    expect(harness.statReads).toEqual([12345, 12345]);
    // Pid reused by a different process: start ticks tell them apart.
    harness.server.pane("%1").pid = 12345;
    expect(await harness.runtime.probe(locator)).toEqual({
      status: "dead",
      reason: "not-recorded-process",
      detail: bothIdentities(`pid 12345 started at ${reissued.startTicks}`),
    });
    // The pid matched, so the stat was read once more -- and disagreed.
    expect(harness.statReads).toEqual([12345, 12345, 12345]);
    // Nothing verified past the identity check: OMP's command line was never consulted.
    expect(harness.cmdlineReads).toEqual([12345]);

    harness.server.windows.clear();
    expect(await harness.runtime.probe(locator)).toEqual({ status: "dead", reason: "gone" });
  });

  it("spawn fails as a launch failure when the pane's process is gone before its identity is read", async () => {
    const harness = await tmuxHarness({
      readProcessStat: async (pid) => {
        throw Object.assign(new Error(`ENOENT /proc/${pid}/stat`), { code: "ENOENT" });
      },
    });
    await expect(harness.runtime.spawn("worker", harness.makeSpec("tester"))).rejects.toThrow(
      /pane %1 for legion-omp-legion-42-tester exited before its process identity could be recorded \(\/proc\/12345\/stat unreadable\)/
    );
  });

  // A `/proc/<pid>/stat` read can fail for two very different reasons. ENOENT/ESRCH is evidence
  // about the process (it exited); anything else -- EACCES, EIO, EMFILE -- is a fault of this
  // host that says nothing about the pane, and reading it as a verdict would make every recorded
  // pane fail verification at once: every root and the controller asked to exit and resurrected,
  // every fresh spawn then failing the same way, with the real cause hidden. So the runtime
  // never turns it into `not-recorded-process`; it propagates out of `probe` and `stop`, and no
  // kill is issued on the way out.
  it.each([
    "EACCES",
    "EIO",
    "EMFILE",
  ])("probe and stop propagate a %s from the /proc stat read instead of reading it as a verdict, and stop issues no kill", async (code) => {
    let fault: (NodeJS.ErrnoException & { code: string }) | undefined;
    let harness: TmuxHarness | undefined;
    harness = await tmuxHarness({
      readProcessStat: async (pid) => {
        if (fault) throw fault;
        if (!harness) throw new Error("harness not ready");
        return harness.server.procStat(pid);
      },
    });
    const locator = await harness.runtime.spawn("worker", harness.makeSpec("tester"));
    expect(await harness.runtime.probe(locator)).toEqual({ status: "alive", pid: 12345 });
    harness.server.commands.length = 0;

    fault = Object.assign(new Error(`${code}: /proc/12345/stat`), { code });
    await expect(harness.runtime.probe(locator)).rejects.toBe(fault);
    await expect(harness.runtime.stop(locator, 50)).rejects.toBe(fault);
    // Both attempts got as far as the pane's own pid (the stat read comes after it), and no
    // further: the pane still runs its recorded pid, and nothing killed it.
    expect(harness.server.commands).toEqual([...verifyArgv("%1"), ...verifyArgv("%1")]);
    expect(harness.server.pane("%1").pid).toBe(12345);

    // The fault clears: the same locator verifies again -- nothing about it was rewritten.
    fault = undefined;
    expect(await harness.runtime.probe(locator)).toEqual({ status: "alive", pid: 12345 });
  });

  it.each([
    "ENOENT",
    "ESRCH",
  ])("probe reads a %s from the /proc stat read as the recorded process being gone, with both identities", async (code) => {
    let fault: (NodeJS.ErrnoException & { code: string }) | undefined;
    let harness: TmuxHarness | undefined;
    harness = await tmuxHarness({
      readProcessStat: async (pid) => {
        if (fault) throw fault;
        if (!harness) throw new Error("harness not ready");
        return harness.server.procStat(pid);
      },
    });
    const locator = await harness.runtime.spawn("worker", harness.makeSpec("tester"));
    if (locator.runtime !== "tmux") throw new Error("tmux locator");
    fault = Object.assign(new Error(`${code}: /proc/12345/stat`), { code });
    expect(await harness.runtime.probe(locator)).toEqual({
      status: "dead",
      reason: "not-recorded-process",
      detail: expect.stringMatching(
        new RegExp(`pid 12345.*recorded pid 12345 start ${locator.paneStartTicks}`)
      ),
    });
  });

  it("probe propagates an EACCES from the /proc cmdline read, and reads an ENOENT there as the process no longer being OMP", async () => {
    // The OMP check runs after a successful stat read, so the process existed a moment ago: a
    // cmdline that has vanished since (ENOENT) means it exited in between -- not OMP any more.
    // Any other read error is the host's fault, never evidence about the pane, and propagates.
    let fault: (NodeJS.ErrnoException & { code: string }) | undefined;
    const harness = await tmuxHarness({
      readProcessCmdline: async () => {
        if (fault) throw fault;
        return "omp\0";
      },
    });
    const locator = await harness.runtime.spawn("worker", harness.makeSpec("tester"));
    if (locator.runtime !== "tmux") throw new Error("tmux locator");
    fault = Object.assign(new Error("EACCES: /proc/12345/cmdline"), { code: "EACCES" });
    await expect(harness.runtime.probe(locator)).rejects.toBe(fault);
    fault = Object.assign(new Error("ENOENT: /proc/12345/cmdline"), { code: "ENOENT" });
    expect(await harness.runtime.probe(locator)).toEqual({
      status: "dead",
      reason: "not-recorded-process",
      detail: expect.stringMatching(
        new RegExp(`pid 12345.*recorded pid 12345 start ${locator.paneStartTicks}`)
      ),
    });
    fault = undefined;
    expect(await harness.runtime.probe(locator)).toEqual({ status: "alive", pid: 12345 });
  });

  it("graceful stop dials the socket without negotiating, sends shutdown, and kills the pane when the close never comes", async () => {
    const harness = await tmuxHarness({ neverCloses: true });
    const locator = await harness.runtime.spawn("worker", harness.makeSpec("tester"));
    harness.server.commands.length = 0;
    await harness.runtime.stop(locator, 50);
    expect(harness.socketPaths).toEqual([
      locator.runtime === "tmux" ? (locator.socketPath ?? "") : "",
    ]);
    const [client] = harness.clients();
    expect(client?.negotiated).toBe(false);
    expect(client?.shutdowns).toBe(1);
    expect(harness.server.commands).toEqual([
      ...verifyArgv("%1"),
      tmuxArgv("kill-pane", "-t", "%1"),
    ]);
  });

  it("graceful stop that the shim confirms never touches the pane", async () => {
    const harness = await tmuxHarness({ hangSleep: true });
    const locator = await harness.runtime.spawn("worker", harness.makeSpec("tester"));
    harness.server.commands.length = 0;
    await harness.runtime.stop(locator, 50);
    expect(harness.clients()[0]?.shutdowns).toBe(1);
    expect(harness.server.commands).toEqual([]);
  });

  it("stop goes straight to kill-pane when the dial fails or the locator has no socket", async () => {
    const harness = await tmuxHarness({ connectFails: true });
    const locator = await harness.runtime.spawn("worker", harness.makeSpec("tester"));
    harness.server.commands.length = 0;
    await harness.runtime.stop(locator, 50);
    expect(harness.dials()).toBe(1);
    expect(harness.server.commands).toEqual([
      ...verifyArgv("%1"),
      tmuxArgv("kill-pane", "-t", "%1"),
    ]);

    const socketless = await harness.runtime.spawn("worker", harness.makeSpec("reviewer"));
    harness.server.commands.length = 0;
    await harness.runtime.stop({ ...socketless, socketPath: undefined } as Locator, 50);
    expect(harness.dials()).toBe(1);
    expect(harness.server.commands).toEqual([
      ...verifyArgv("%2"),
      tmuxArgv("kill-pane", "-t", "%2"),
    ]);
  });

  it("stop never kills a pane whose process is not the one recorded, logging both identities once, and stays silent when the caller already decided", async () => {
    const harness = await tmuxHarness();
    const locator = await harness.runtime.spawn("worker", harness.makeSpec("tester"));
    if (locator.runtime !== "tmux" || !locator.tmuxPaneId) throw new Error("tmux locator");
    const reissued = harness.server.reissuePane(locator.tmuxPaneId);
    const logged: string[] = [];
    const consoleError = spyOn(console, "error").mockImplementation((...args: unknown[]) => {
      logged.push(args.map(String).join(" "));
    });
    try {
      harness.server.commands.length = 0;
      await harness.runtime.stop(locator, 50, { skipGraceful: true });
      // Verification stopped at the pid mismatch: the pane's row was read, nothing was signalled.
      expect(harness.server.commands).toEqual([
        tmuxArgv("list-panes", "-t", "%1", "-F", "#{pane_id} #{pane_pid}"),
      ]);
      expect(logged).toHaveLength(1);
      expect(logged[0]).toContain("not killing pane %1");
      expect(logged[0]).toContain(`now runs pid ${reissued.pid}`);
      expect(logged[0]).toContain(
        `recorded pid ${locator.panePid} start ${locator.paneStartTicks}`
      );
      // The stranger is untouched.
      expect(harness.server.pane("%1").pid).toBe(reissued.pid);

      harness.server.commands.length = 0;
      await harness.runtime.stop(locator, 50, { skipGraceful: true, refuseKill: true });
      expect(harness.server.commands).toEqual([]);
      expect(logged).toHaveLength(1);
    } finally {
      consoleError.mockRestore();
    }
  });

  it("stop on a legacy locator without identity sends the graceful shutdown over its socket and never kills", async () => {
    const harness = await tmuxHarness({ neverCloses: true });
    const spawned = await harness.runtime.spawn("worker", harness.makeSpec("tester"));
    if (spawned.runtime !== "tmux") throw new Error("tmux locator");
    const { panePid: _pid, paneStartTicks: _ticks, ...legacy } = spawned;
    const consoleError = spyOn(console, "error").mockImplementation(() => {});
    try {
      harness.server.commands.length = 0;
      await harness.runtime.stop(legacy, 50);
      expect(harness.clients()[0]?.shutdowns).toBe(1);
      expect(harness.server.commands).toEqual([]);
      expect(consoleError).toHaveBeenCalledTimes(1);
      expect(String(consoleError.mock.calls[0]?.[0])).toContain("no recorded process identity");
      expect(harness.server.windowOf("%1")).toBe("@42");
    } finally {
      consoleError.mockRestore();
    }
  });

  it("stop tolerates a pane that is already gone and surfaces every other kill-pane failure", async () => {
    const harness = await tmuxHarness();
    const locator = await harness.runtime.spawn("worker", harness.makeSpec("tester"));
    harness.server.killPaneResult = { exitCode: 1, stderr: "can't find pane: %1" };
    await expect(
      harness.runtime.stop(locator, 50, { skipGraceful: true })
    ).resolves.toBeUndefined();
    harness.server.killPaneResult = { exitCode: 1, stderr: "no server running on /tmp/tmux-1/x" };
    await expect(
      harness.runtime.stop(locator, 50, { skipGraceful: true })
    ).resolves.toBeUndefined();

    harness.server.killPaneResult = { exitCode: 1, stderr: "tmux: server not responding" };
    const failure = await harness.runtime.stop(locator, 50, { skipGraceful: true }).then(
      () => undefined,
      (error: unknown) => error
    );
    expect(failure).toBeInstanceOf(ProcessStopFailed);
    expect((failure as ProcessStopFailed).message).toBe(
      "kill-pane %1 exited 1: tmux: server not responding"
    );
    expect((failure as ProcessStopFailed).locator).toBe(locator);
  });

  it("stop on a locator without a pane id never throws: the graceful shutdown still goes out over its socket, the kill is refused as for any identity-less locator", async () => {
    // B4: a pane-id-less record is an identity-less locator like any other. `probe` already says
    // so without touching tmux; `stop` must agree instead of throwing above the gate and leaving
    // a closing tree lingering forever.
    const harness = await tmuxHarness({ hangSleep: true });
    const spawned = await harness.runtime.spawn("worker", harness.makeSpec("tester"));
    if (spawned.runtime !== "tmux") throw new Error("tmux locator");
    const { tmuxPaneId: _pane, panePid: _pid, paneStartTicks: _ticks, ...paneless } = spawned;
    expect(await harness.runtime.probe(paneless)).toEqual({
      status: "dead",
      reason: "not-recorded-process",
      detail: "pane @42 has no recorded process identity (locator predates identity tracking)",
    });
    const consoleError = spyOn(console, "error").mockImplementation(() => {});
    try {
      harness.server.commands.length = 0;
      // The shim accepts the shutdown and closes: a confirmed graceful stop, nothing to kill.
      await expect(harness.runtime.stop(paneless, 50)).resolves.toBeUndefined();
      expect(harness.clients()[0]?.shutdowns).toBe(1);
      expect(harness.server.commands).toEqual([]);
      expect(consoleError).not.toHaveBeenCalled();

      // The caller's own probe already decided: nothing to verify, nothing to log again.
      await expect(
        harness.runtime.stop(paneless, 50, { skipGraceful: true, refuseKill: true })
      ).resolves.toBeUndefined();
      expect(harness.server.commands).toEqual([]);
      expect(consoleError).not.toHaveBeenCalled();

      // Deciding itself: refused once, logged once, no tmux command issued for a pane it cannot name.
      await expect(
        harness.runtime.stop(paneless, 50, { skipGraceful: true })
      ).resolves.toBeUndefined();
      expect(harness.server.commands).toEqual([]);
      expect(consoleError).toHaveBeenCalledTimes(1);
      expect(String(consoleError.mock.calls[0]?.[0])).toContain("no recorded process identity");
    } finally {
      consoleError.mockRestore();
    }
    // The pane the paneless record could not name is untouched.
    expect(harness.server.windowOf("%1")).toBe("@42");
  });

  it("stop throws ProcessStopFailed, killing nothing, when list-panes itself fails for a reason that does not prove the pane gone", async () => {
    // B3: a failed listing is not a missing pane. Before this, `stop` returned normally here and
    // the caller cleared the locator of a possibly-live process.
    const harness = await tmuxHarness();
    const locator = await harness.runtime.spawn("worker", harness.makeSpec("tester"));
    for (const [listing, detail] of [
      [{ exitCode: 1, stderr: "" }, "list-panes -t %1 exited 1"],
      [
        { exitCode: 1, stderr: "tmux: server not responding" },
        "list-panes -t %1 exited 1: tmux: server not responding",
      ],
    ] as const) {
      harness.server.listPanesResult = listing;
      harness.server.commands.length = 0;
      const failure = await harness.runtime.stop(locator, 50, { skipGraceful: true }).then(
        () => undefined,
        (error: unknown) => error
      );
      expect(failure).toBeInstanceOf(ProcessStopFailed);
      // The same sentence `probe` rejects with for this listing (see the probe row below): the
      // argv, exit code, and stderr are the contract; the pane id names what could not be seen.
      expect((failure as ProcessStopFailed).message).toBe(`cannot verify pane %1: ${detail}`);
      expect((failure as ProcessStopFailed).locator).toBe(locator);
      expect(harness.server.commands).toEqual([
        tmuxArgv("list-panes", "-t", "%1", "-F", "#{pane_id} #{pane_pid}"),
      ]);
    }
    // The pane and its process are exactly as they were.
    expect(harness.server.pane("%1").pid).toBe(12345);
    expect(await harness.runtime.probe(locator)).toEqual({ status: "alive", pid: 12345 });
  });

  it.each([
    ["can't find pane: %1"],
    ["no server running on /tmp/tmux-1000/legion-omp"],
    ["error connecting to /tmp/tmux-1000/legion-omp (No such file or directory)"],
  ])("stop treats a list-panes failure whose stderr proves the pane gone as an already-gone pane: %s", async (stderr) => {
    const harness = await tmuxHarness();
    const locator = await harness.runtime.spawn("worker", harness.makeSpec("tester"));
    harness.server.listPanesResult = { exitCode: 1, stderr };
    harness.server.commands.length = 0;
    await expect(
      harness.runtime.stop(locator, 50, { skipGraceful: true })
    ).resolves.toBeUndefined();
    expect(harness.server.commands).toEqual([
      tmuxArgv("list-panes", "-t", "%1", "-F", "#{pane_id} #{pane_pid}"),
    ]);
    harness.server.listPanesResult = { exitCode: 1, stderr };
    expect(await harness.runtime.probe(locator)).toEqual({ status: "dead", reason: "gone" });
  });

  it("probe throws, never alive and never gone, when list-panes fails for a reason that proves nothing", async () => {
    const harness = await tmuxHarness();
    const locator = await harness.runtime.spawn("worker", harness.makeSpec("tester"));
    harness.server.listPanesResult = { exitCode: 1, stderr: "tmux: server not responding" };
    await expect(harness.runtime.probe(locator)).rejects.toThrow(
      "cannot verify pane %1: list-panes -t %1 exited 1: tmux: server not responding"
    );
    // Nothing about the pane changed; the next successful listing verifies it again.
    expect(await harness.runtime.probe(locator)).toEqual({ status: "alive", pid: 12345 });
  });

  it("refuses to operate a kubernetes locator", async () => {
    const harness = await tmuxHarness();
    const foreign: Locator = {
      runtime: "kubernetes",
      namespace: "legion",
      podName: "legion-legion-42-tester-g1",
      podUid: "uid-1",
      pvcName: "legion-legion-42",
      roleToken: "legion-omp-legion-42-tester",
    };
    for (const attempt of [
      () => harness.runtime.probe(foreign),
      () => harness.runtime.connect(foreign),
      () => harness.runtime.stop(foreign, 50),
    ]) {
      await expect(attempt()).rejects.toThrow("tmux runtime cannot operate a kubernetes locator");
    }
  });

  it("connect dials the locator's socket with the given timeout and refuses a socket-less one", async () => {
    const harness = await tmuxHarness();
    const locator = await harness.runtime.spawn("worker", harness.makeSpec("tester"));
    await harness.runtime.connect(locator, 1_234);
    expect(harness.socketPaths).toEqual([
      locator.runtime === "tmux" ? (locator.socketPath ?? "") : "",
    ]);
    expect(harness.timeouts).toEqual([1_234]);
    await expect(
      harness.runtime.connect({ ...locator, socketPath: undefined } as Locator)
    ).rejects.toThrow("tmux locator @42 has no shim socket");
  });

  it("reconcileOrphans kills unknown owned windows and unknown panes, sparing exempt windows and unowned ones", async () => {
    const harness = await tmuxHarness({ now: () => 10_000_000 });
    harness.server.sessionExists = true;
    harness.server.addWindow("@42", "legion-omp", 2); // %1 known, %2 an unrecorded split
    harness.server.addWindow("@77", "legion-omp", 2); // %3 %4: window known, no pane id recorded
    harness.server.addWindow("@90", "legion-omp", 1); // %5: an orphaned Legion window
    harness.server.addWindow("@91", undefined, 1); // %6: a human's window in the same session
    const recorded: Locator[] = [
      { runtime: "tmux", tmuxSession: "legion-omp", tmuxWindowId: "@42", tmuxPaneId: "%1" },
      { runtime: "tmux", tmuxSession: "legion-omp", tmuxWindowId: "@77" },
    ];
    const known = new Set(recorded.flatMap((locator) => locatorHandles(locator)));
    expect(known).toEqual(new Set(["@42", "%1", "@77", "@77/*"]));
    await harness.runtime.reconcileOrphans(known, 120_000);
    const kills = harness.server.commands.filter((c) => c[3]?.startsWith("kill-"));
    expect(kills).toEqual([
      tmuxArgv("kill-window", "-t", "@90"),
      tmuxArgv("kill-pane", "-t", "%2"),
    ]);
    expect([...harness.server.windows.keys()]).toEqual(["@42", "@77", "@91"]);
    expect([...(harness.server.windows.get("@42")?.panes.keys() ?? [])]).toEqual(["%1"]);
    expect([...(harness.server.windows.get("@77")?.panes.keys() ?? [])]).toEqual(["%3", "%4"]);
  });

  it("reconcileOrphans leaves anything younger than the grace period alone", async () => {
    const harness = await tmuxHarness({ now: () => 0 });
    harness.server.sessionExists = true;
    harness.server.addWindow("@90", "legion-omp", 1);
    await harness.runtime.reconcileOrphans(new Set(), 120_000);
    expect(harness.server.commands.filter((c) => c[3]?.startsWith("kill-"))).toEqual([]);
    await harness.runtime.reconcileOrphans(new Set(), 0);
    expect(harness.server.commands.filter((c) => c[3]?.startsWith("kill-"))).toEqual([
      tmuxArgv("kill-window", "-t", "@90"),
    ]);
  });
});

describe("KubernetesRuntime", () => {
  it("adopts the working copy for the assigned role on a live-idle re-prompt: the adopt-working-copy frame goes to the pod's registered stream under the role's identity, and the shim's failure is the adoption's", async () => {
    // The pod's workspace exists only on its volume: the init container adopted once with the
    // pod's identity at start; a later assignment for the same live pod must adopt again through
    // its shim, or the next describe lands under whatever identity last touched `@`.
    const harness = await kubernetesHarness();
    const locator = await harness.runtime.spawn("worker", harness.makeSpec("implementer"));
    if (locator.runtime !== "kubernetes") throw new Error("kubernetes locator");
    const client = harness.clients()[0];
    if (!client) throw new Error("no registered stream");
    const identity = {
      jjUser: "legion-implementer[bot]",
      jjEmail: "implementer@users.noreply.github.com",
    };
    await harness.runtime.adoptWorkingCopy(issue, "implementer", identity, 300_000);
    expect(client.adoptions).toEqual([
      {
        jjUser: "legion-implementer[bot]",
        jjEmail: "implementer@users.noreply.github.com",
        timeoutMs: 300_000,
      },
    ]);
    client.adoptImpl = async () => {
      throw new Error("Command failed (exit 1): jj metaedit …\nno such revision");
    };
    await expect(
      harness.runtime.adoptWorkingCopy(issue, "implementer", identity, 300_000)
    ).rejects.toThrow(
      "Could not adopt LEGION-42's working copy for implementer: Command failed (exit 1): jj metaedit"
    );
    // A role whose stream is not registered cannot be adopted (nor prompted): a failure, never a skip.
    await expect(
      harness.runtime.adoptWorkingCopy(issue, "tester", identity, 300_000)
    ).rejects.toThrow(
      `Could not adopt LEGION-42's working copy for tester: no worker stream is registered for ${roleToken("omp", issue, "tester")}`
    );
  });
});

describe("ProcessManager stays runtime-agnostic", () => {
  const source = readFileSync(path.resolve(import.meta.dir, "../processes.ts"), "utf8");

  // Every name here is a tmux-side symbol that exists today (`runtime-tmux.ts`/`tmux.ts`) and
  // must never migrate back into the manager; a name nothing defines proves nothing.
  it.each([
    "tmux.",
    "prepareSocket",
    "preparePane",
    "verifyPaneProcess",
    "probedWindowId",
    "lookupPane",
    "PANE_GONE_STDERR",
  ])("processes.ts never references %s", (symbol) => {
    expect(source.split(symbol).length - 1).toBe(0);
  });

  it("processes.ts never branches on locator.runtime", () => {
    expect(/locator\.runtime|runtime ===/.test(source)).toBe(false);
  });

  it("processes.ts imports no tmux or secret-writing module", () => {
    expect(source).not.toMatch(
      /from "\.\/tmux"|writeSecretFile|connectWorkerRpc|readProcessCmdline/
    );
  });

  it("processes.ts reads no runtime-specific locator field or type", () => {
    expect(source).not.toMatch(
      /tmuxPaneId|tmuxWindowId|tmuxSession|socketPath|podName|podUid|TmuxLocator|K8sLocator|runtime-tmux/
    );
  });

  it("ProcessManager's members carry no tmux vocabulary", () => {
    // Identifiers only — class members (`private ... name`, `name(`), locals (`const|let name`),
    // and `ProcessManagerDeps` fields — never comments or string handles a runtime hands back.
    const identifiers = new Set<string>();
    for (const match of source.matchAll(
      /^\s*(?:private |protected |readonly |async |static |get )*([A-Za-z_$][\w$]*)\s*[(:=?]/gm
    )) {
      identifiers.add(match[1] as string);
    }
    for (const match of source.matchAll(/\b(?:const|let)\s+([A-Za-z_$][\w$]*)/g)) {
      identifiers.add(match[1] as string);
    }
    expect([...identifiers].filter((name) => /pane|window/i.test(name))).toEqual([]);
  });
});

describe("runtime helpers", () => {
  it("boundedWait releases its real timer on cancel", async () => {
    const { timedOut, cancel } = boundedWait(60_000);
    cancel();
    // Without the cancel this would hold the test for a minute; the cancelled sleep resolves.
    expect(await timedOut).toBe(true);
  });

  it("boundedWait defers to an injected sleep, whose cancel is a no-op", async () => {
    const slept: number[] = [];
    const { timedOut, cancel } = boundedWait(7, async (ms) => {
      slept.push(ms);
    });
    cancel();
    expect(await timedOut).toBe(true);
    expect(slept).toEqual([7]);
  });

  it("awaitShutdown sends the frame before it creates the wait, and confirms a clean close", async () => {
    const order: string[] = [];
    const client = fakeWorkerRpcClient();
    const realShutdown = client.shutdown.bind(client);
    client.shutdown = () => {
      order.push("shutdown");
      realShutdown();
    };
    // The fake client's `shutdown` closes on a microtask; a sleep that never settles proves the
    // clean close alone confirms the stop, and the recorded order proves the frame went first.
    const confirmed = await awaitShutdown(client, 50, async () => {
      order.push("sleep");
      await new Promise<void>(() => {});
    });
    expect(confirmed).toBe(true);
    expect(order).toEqual(["shutdown", "sleep"]);
  });

  it("awaitShutdown treats a timeout and a rejected close alike: unconfirmed", async () => {
    const silent = fakeWorkerRpcClient();
    silent.shutdown = () => {};
    expect(await awaitShutdown(silent, 50, async () => {})).toBe(false);

    const closed = Promise.withResolvers<void>();
    const rejecting: WorkerRpcClient = { ...fakeWorkerRpcClient(), closed: closed.promise };
    rejecting.shutdown = () => closed.reject(new Error("reset"));
    expect(await awaitShutdown(rejecting, 60_000)).toBe(false);
  });

  it("probeWorker forwards connect failures and get_state outcomes like probeWorkerSocket", async () => {
    const connectError = new Error("ECONNREFUSED");
    const refused = await probeWorker(async () => {
      throw connectError;
    }, 50);
    expect(refused).toEqual({
      client: undefined,
      connectError,
      stateAnswered: false,
      stateError: undefined,
    });

    const busy = fakeWorkerRpcClient();
    const stateError = new Error("get_state timed out");
    busy.getStateImpl = async () => {
      throw stateError;
    };
    const busyProbe = await probeWorker(async () => busy, 50);
    expect(busyProbe.client).toBe(busy);
    expect(busyProbe.stateAnswered).toBe(false);
    expect(busyProbe.stateError).toBe(stateError);

    const idle = fakeWorkerRpcClient();
    const idleProbe = await probeWorker(async () => idle, 50);
    expect(idleProbe).toEqual({
      client: idle,
      connectError: undefined,
      stateAnswered: true,
      stateError: undefined,
    });
    expect(idle.getStateCalls).toBe(1);
  });
});

import { afterAll, describe, expect, it } from "bun:test";
import { mkdtemp, readFile, rm, stat } from "node:fs/promises";
import os from "node:os";
import path from "node:path";
import { type IssueKey, type LegionRole, roleToken } from "@legion/contracts";
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
import { TmuxRuntime } from "../runtime-tmux";
import type { WorkerRpcClient } from "../worker-rpc";
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
}

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
    ...(role === "controller" ? {} : { issue: forIssue ?? issue }),
    role,
    workspaceDir: "/work",
    env: { LEGION_ROLE: role, UNSET: undefined },
    innerCommand: "omp --mode rpc",
    secrets:
      role === "controller"
        ? { LEGION_CONTROLLER_SECRET: "controller-secret" }
        : { LEGION_BOOT_TOKEN: "boot-token" },
  };
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
  };
}

interface FakePane {
  pid: number;
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
  /** Overrides the next `kill-pane`'s result (exit code and stderr) when set. */
  killPaneResult: { exitCode: number; stderr?: string } | undefined;
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
      const pid = this.nextPid;
      this.nextPid += 1;
      panes.set(paneId, {
        pid,
        startCommand: "legion worker-shim --socket x -- omp",
        activityAt: 0,
      });
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

  pids(): Set<number> {
    const pids = new Set<number>();
    for (const window of this.windows.values()) {
      for (const pane of window.panes.values()) pids.add(pane.pid);
    }
    return pids;
  }

  run = async (
    command: string[]
  ): Promise<{ stdout: string; stderr?: string; exitCode: number }> => {
    this.commands.push(command);
    if (command[0] === "kill") {
      return { stdout: "", exitCode: this.pids().has(Number(command[2])) ? 0 : 1 };
    }
    if (command[0] !== "tmux") return { stdout: "", exitCode: 0 };
    const verb = command[3];
    const target = command[command.indexOf("-t") + 1];
    switch (verb) {
      case "has-session":
        return { stdout: "", exitCode: this.sessionExists ? 0 : 1 };
      case "new-session":
        this.sessionExists = true;
        return { stdout: "", exitCode: 0 };
      case "set-option":
        if (command.includes("-w")) {
          const window = this.windows.get(target);
          if (!window) return { stdout: "no such window", exitCode: 1 };
          window.owner = command[command.length - 1];
        }
        return { stdout: "", exitCode: 0 };
      case "new-window": {
        const windowId = `@${this.nextWindow}`;
        this.nextWindow += 1;
        const paneId = `%${this.nextPane}`;
        this.nextPane += 1;
        const pid = this.nextPid;
        this.nextPid += 1;
        this.windows.set(windowId, {
          activityAt: 0,
          panes: new Map([
            [paneId, { pid, startCommand: command[command.length - 1] ?? "", activityAt: 0 }],
          ]),
        });
        return { stdout: `${windowId} ${paneId} ${pid}\n`, exitCode: 0 };
      }
      case "split-window": {
        const window = this.windows.get(target);
        if (!window) return { stdout: "can't find window", exitCode: 1 };
        const paneId = `%${this.nextPane}`;
        this.nextPane += 1;
        const pid = this.nextPid;
        this.nextPid += 1;
        window.panes.set(paneId, {
          pid,
          startCommand: command[command.length - 1] ?? "",
          activityAt: 0,
        });
        return { stdout: `${paneId} ${pid}\n`, exitCode: 0 };
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
  /** Every locator `spawn` returned, in order — what `issueLocators` hands the runtime. */
  locators: Locator[];
  socketPaths: string[];
  /** The `timeoutMs` each dial was given. */
  timeouts: Array<number | undefined>;
  persistCalls: () => number;
  cmdlineReads: number[];
}

async function tmuxHarness(
  options: HarnessOptions & {
    /** Every dial is refused, as against a shim that is already gone. */
    connectFails?: boolean;
    /** A graceful stop's timeout never fires, so only a real close can end the wait. */
    hangSleep?: boolean;
    readProcessCmdline?: (pid: number) => Promise<string>;
    now?: () => number;
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
  const issueOf = new Map<Locator, IssueKey>();
  let persistCalls = 0;
  const runtime = new TmuxRuntime({
    tmux: { run: server.run, socket: "legion-omp" },
    project: "omp",
    stateDir,
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
    issueLocators: (forIssue) =>
      locators.filter((locator) => locator.runtime === "tmux" && issueOf.get(locator) === forIssue),
    persist: async () => {
      persistCalls += 1;
    },
  });
  // Records every spawned locator by issue so `issueLocators` sees what state would.
  const spawningRuntime: Runtime = {
    spawn: async (kind, spec) => {
      const locator = await runtime.spawn(kind, spec);
      if (spec.issue) {
        issueOf.set(locator, spec.issue);
        locators.push(locator);
      }
      return locator;
    },
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
    server,
    stateDir,
    locators,
    socketPaths,
    timeouts,
    persistCalls: () => persistCalls,
    cmdlineReads,
  };
}

const harnesses: Array<[string, (options?: HarnessOptions) => Promise<Harness>]> = [
  ["FakeRuntime", fakeHarness],
  ["TmuxRuntime", tmuxHarness],
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
  const socketFor = (stateDir: string, name: string): string =>
    path.join(stateDir, "workers", `${name}.sock`);
  const shimCommand = (workspaceDir: string, socketPath: string, innerCommand: string): string =>
    `cd ${workspaceDir} && ${process.execPath} ${DAEMON_CLI_ENTRYPOINT} worker-shim --socket ${socketPath} -- ${innerCommand}`;

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
      tmuxArgv("new-session", "-d", "-s", "legion-omp", "-n", "__legion_bootstrap", "sleep 3600"),
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
        `LEGION_BOOT_TOKEN_FILE=${bootTokenFile}`,
        shimCommand("/work", socketPath, "omp --mode rpc")
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
    });
    expect(await readFile(bootTokenFile, "utf8")).toBe("boot-token");
    expect((await stat(bootTokenFile)).mode & 0o777).toBe(0o600);
    expect((await stat(path.dirname(bootTokenFile))).mode & 0o777).toBe(0o700);
  });

  it("splits a second process for the same issue into its recorded window", async () => {
    const harness = await tmuxHarness();
    await harness.runtime.spawn("root", harness.makeSpec("architect"));
    harness.server.commands.length = 0;
    const worker = await harness.runtime.spawn("worker", harness.makeSpec("implementer"));
    const socketPath = socketFor(harness.stateDir, "implementer-9e2fb104");
    expect(harness.server.commands).toEqual([
      tmuxArgv("list-panes", "-t", "@42", "-F", "#{pane_id}"),
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
        `LEGION_BOOT_TOKEN_FILE=${path.join(harness.stateDir, "secrets", roleToken("omp", issue, "implementer"))}`,
        shimCommand("/work", socketPath, "omp --mode rpc")
      ),
      tmuxArgv("select-layout", "-t", "@42", "tiled"),
    ]);
    expect(worker).toEqual({
      runtime: "tmux",
      tmuxSession: "legion-omp",
      tmuxWindowId: "@42",
      tmuxPaneId: "%2",
      socketPath,
    });
  });

  it("opens a fresh window when the recorded one is dead and rewrites every recorded locator to it", async () => {
    const harness = await tmuxHarness();
    const root = await harness.runtime.spawn("root", harness.makeSpec("architect"));
    const first = await harness.runtime.spawn("worker", harness.makeSpec("planner"));
    expect(first.runtime === "tmux" && first.tmuxWindowId).toBe("@42");
    // A human killed the issue's window out from under the daemon.
    harness.server.windows.delete("@42");
    const next = await harness.runtime.spawn("worker", harness.makeSpec("implementer"));
    expect(next.runtime === "tmux" && next.tmuxWindowId).toBe("@43");
    expect(harness.server.commands.filter((c) => c[3] === "new-window")).toHaveLength(2);
    for (const locator of [root, first]) {
      expect(locator.runtime === "tmux" && locator.tmuxWindowId).toBe("@43");
    }
    // And a fourth spawn splits into the rewritten window rather than opening a third.
    const another = await harness.runtime.spawn("worker", harness.makeSpec("tester"));
    expect(another.runtime === "tmux" && another.tmuxWindowId).toBe("@43");
    expect(harness.server.commands.filter((c) => c[3] === "new-window")).toHaveLength(2);
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
        shimCommand("/work", socketPath, "omp --mode rpc")
      )
    );
    expect(locator).toEqual({
      runtime: "tmux",
      tmuxSession: "legion-omp",
      tmuxWindowId: "@42",
      tmuxPaneId: "%1",
      socketPath,
    });
    expect(await readFile(secretFile, "utf8")).toBe("controller-secret");
    expect((await stat(secretFile)).mode & 0o777).toBe(0o600);
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

  it("probe backfills a missing pane id from the window's first pane and persists once", async () => {
    const harness = await tmuxHarness();
    harness.server.sessionExists = true;
    harness.server.addWindow("@42", "legion-omp", 1);
    const locator: Locator = { runtime: "tmux", tmuxSession: "legion-omp", tmuxWindowId: "@42" };
    expect(await harness.runtime.probe(locator)).toEqual({ status: "alive", pid: 12345 });
    expect(locator.tmuxPaneId).toBe("%1");
    expect(harness.persistCalls()).toBe(1);
    expect(await harness.runtime.probe(locator)).toEqual({ status: "alive", pid: 12345 });
    expect(harness.persistCalls()).toBe(1);
  });

  it("probe reads the watched pane's own pid from its window listing, never a sibling's", async () => {
    const harness = await tmuxHarness({
      // Only the architect's pane (the window's first) still runs OMP.
      readProcessCmdline: async (pid) => (pid === 12345 ? "omp\0" : "sleep\0"),
    });
    harness.server.sessionExists = true;
    const [, worker] = harness.server.addWindow("@1464", "legion-omp", 3);
    const locator: Locator = {
      runtime: "tmux",
      tmuxSession: "legion-omp",
      tmuxWindowId: "@1464",
      tmuxPaneId: worker,
    };
    expect(await harness.runtime.probe(locator)).toEqual({ status: "dead" });
    expect(harness.cmdlineReads).toEqual([12346]);
    expect(harness.persistCalls()).toBe(0);
  });

  it("probe reports a pane that is gone, or one no longer running OMP, as dead", async () => {
    const harness = await tmuxHarness({ readProcessCmdline: async () => "bash\0" });
    const locator = await harness.runtime.spawn("worker", harness.makeSpec("tester"));
    expect(await harness.runtime.probe(locator)).toEqual({ status: "dead" });
    harness.server.windows.clear();
    expect(await harness.runtime.probe(locator)).toEqual({ status: "dead" });
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
    expect(harness.server.commands).toEqual([tmuxArgv("kill-pane", "-t", "%1")]);
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
    expect(harness.server.commands).toEqual([tmuxArgv("kill-pane", "-t", "%1")]);

    const socketless = await harness.runtime.spawn("worker", harness.makeSpec("reviewer"));
    harness.server.commands.length = 0;
    await harness.runtime.stop({ ...socketless, socketPath: undefined } as Locator, 50);
    expect(harness.dials()).toBe(1);
    expect(harness.server.commands).toEqual([tmuxArgv("kill-pane", "-t", "%2")]);
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

    const paneless = await harness.runtime
      .stop({ ...locator, tmuxPaneId: undefined } as Locator, 50, { skipGraceful: true })
      .then(
        () => undefined,
        (error: unknown) => error
      );
    expect(paneless).toBeInstanceOf(Error);
    expect(paneless).not.toBeInstanceOf(ProcessStopFailed);
    expect((paneless as Error).message).toMatch(/missing a pane id/);
  });

  it("refuses to operate a kubernetes locator", async () => {
    const harness = await tmuxHarness();
    const foreign: Locator = {
      runtime: "kubernetes",
      namespace: "legion",
      podName: "legion-legion-42-tester-g1",
      podUid: "uid-1",
      pvcName: "legion-legion-42",
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

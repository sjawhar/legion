import { afterAll, describe, expect, it, spyOn } from "bun:test";
import { readFileSync } from "node:fs";
import { mkdtemp, readFile, rm, stat } from "node:fs/promises";
import os from "node:os";
import path from "node:path";
import { type IssueKey, type LegionRole, roleToken } from "@legion/contracts";
import {
  awaitShutdown,
  boundedWait,
  DAEMON_CLI_ENTRYPOINT,
  describeProcessLocation,
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
import { procStatLine } from "./ci-fixtures";
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
    issueLocators: (forIssue) =>
      locators.filter((locator) => locator.runtime === "tmux" && issueOf.get(locator) === forIssue),
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
        `LEGION_BOOT_TOKEN_FILE=${path.join(harness.stateDir, "secrets", roleToken("omp", issue, "implementer"))}`,
        shimCommand("/work", socketPath, "omp --mode rpc")
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
      panePid: 12345,
      paneStartTicks: startTicksOf(12345),
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

describe("describeProcessLocation", () => {
  it("names a tmux window and the attach command when the locator predates pane ids, never `pane undefined`", () => {
    const described = describeProcessLocation({
      runtime: "tmux",
      tmuxSession: "legion-omp",
      tmuxWindowId: "@7",
    });
    expect(described).toBe(
      "window @7 of tmux server legion-omp (attach with `tmux -L legion-omp attach -t legion-omp`)"
    );
    expect(described).not.toContain("undefined");
  });

  it("names a kubernetes pod and its namespace", () => {
    expect(
      describeProcessLocation({
        runtime: "kubernetes",
        namespace: "legion",
        podName: "legion-omp-LEGION-42-1",
        podUid: "uid-1",
        pvcName: "pvc-1",
      })
    ).toBe("pod legion-omp-LEGION-42-1 in namespace legion");
  });
});

import { afterAll, describe, expect, it, vi } from "bun:test";
import { mkdir, mkdtemp, readFile, rm, writeFile } from "node:fs/promises";
import os from "node:os";
import path from "node:path";
import {
  controllerToken,
  formatIssueKey,
  type IssueKey,
  type LegionRole,
  roleToken,
  roleTopic,
} from "@legion/contracts";
import type { DaemonConfig } from "../config";
import { type LegionState, loadState, newLegionState, saveState } from "../legion-state";
import {
  addressingFragment,
  type ControlDirective,
  type ExceptionInfo,
  ProcessManager,
  type ProcessManagerDeps,
} from "../processes";
import type { WorkerRpcClient } from "../worker-rpc";

const root = formatIssueKey("sjawhar", "legion", 42);
const child = formatIssueKey("sjawhar", "legion", 43);
const grandchild = formatIssueKey("sjawhar", "legion", 44);
const tempDirs: string[] = [];

async function temporaryDir(): Promise<string> {
  const directory = await mkdtemp(path.join(os.tmpdir(), "legion-processes-"));
  tempDirs.push(directory);
  return directory;
}

function fakeWorkerRpcClient(): WorkerRpcClient & {
  prompts: string[];
  negotiated: boolean;
  getStateCalls: number;
  getStateImpl?: () => Promise<Record<string, unknown>>;
  emitRunState(state: "running" | "idle"): void;
} {
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
    shutdown() {},
    close() {
      const wasIdle = runState === "idle";
      runState = "idle";
      closed.resolve();
      if (!wasIdle) idleCallback?.();
    },
    onIdle(callback: () => void) {
      idleCallback = callback;
    },
    emitRunState(state: "running" | "idle") {
      const wasIdle = runState === "idle";
      runState = state;
      if (state === "idle" && !wasIdle) idleCallback?.();
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
    boardProjectIds: [],
    repos: ["sjawhar/legion"],
    appLogins: [],
    admissionCap: 1,
    workerCap: 5,
    maxRecursionDepth: 8,
    lingerHours: 2,
    maxFixAttempts: 3,
    resyncIntervalMs: 600_000,
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
      ompSessionFile: "/state/trees/sjawhar-legion-42/.omp/session.json",
    },
    status: "active",
    launchFailures: 0,
    heldEvents: [],
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
  > = {}
): {
  manager: ProcessManager;
  state: LegionState;
  commands: string[][];
  controlRequests: Array<{ subject: string; json: string }>;
  publications: Array<{ subject: string; json: string }>;
} {
  const commands: string[][] = [];
  const publications: Array<{ subject: string; json: string }> = [];
  const controlRequests: Array<{ subject: string; json: string }> = [];
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
  return {
    manager: new ProcessManager(deps),
    state,
    commands,
    controlRequests,
    publications,
  };
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
    const workspace = path.join(stateDir, "workspaces", "sjawhar", "legion", "issue-42");
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
      config: config(stateDir, { dispatchUrl: "http://127.0.0.1:18766" }),
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

    await processes.spawnRoot(root);

    expect(await readFile(path.join(workspace, ".omp", "config.yml"), "utf8")).toBe("");
    expect(state.trees[root]).toMatchObject({
      generation: 1,
      status: "active",
    });
    expect(commands).toEqual([
      ["jj", "git", "fetch", "-R", repo],
      ["git", `--git-dir=${repo}/.git`, "worktree", "prune"],
      ["jj", "workspace", "add", workspace, "--name", "issue-42", "--revision", "main", "-R", repo],
      ["jj", "bookmark", "set", "legion/issue-42", "--allow-backwards"],
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
        "sjawhar__legion-42",
        "-e",
        "LEGION_TREE=sjawhar/legion#42",
        "-e",
        "LEGION_ISSUE=sjawhar/legion#42",
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
        "LEGION_CONTROL_SUBJECT=legion.ctl.sjawhar-legion-42.1",
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
        "DISPATCH_MCP_URL=http://127.0.0.1:18766/mcp",
        `cd ${workspace} && ${process.execPath} ${path.resolve(import.meta.dir, "../../cli/index.ts")} worker-shim --socket ${path.join(stateDir, "workers", "42-architect-edb483d7.sock")} -- /opt/oh-my-pi/18.0.3/omp --mode rpc --append-system-prompt "$(cat ${path.resolve(import.meta.dir, "../../../../pi-envoy")}/roles/architect-root.md)" --append-system-prompt '${addressingFragment("omp", root, root, "architect").replaceAll("'", "'\\''")}'`,
      ],
      ["tmux", "kill-window", "-t", "legion-omp:__legion_bootstrap"],
      ["tmux", "set-option", "-w", "-t", "@42", "@legion_owner", "legion-omp"],
    ]);
    expect(workspaceCalls).toContainEqual({
      command: ["jj", "bookmark", "set", "legion/issue-42", "--allow-backwards"],
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
      LEGION_ROOT_WORKSPACE: path.join(stateDir, "workspaces", "sjawhar", "legion", "issue-42"),
      LEGION_GENERATION: "1",
      LEGION_BOOT_TOKEN: "boot-token",
      LEGION_DAEMON_URL: "http://127.0.0.1:13999",
      LEGION_PROJECT: "omp",
      ENVOY_NATS_URL: "nats://127.0.0.1:4222",
      ENVOY_URL: "http://127.0.0.1:9020",
      LEGION_CONTROL_SUBJECT: "legion.ctl.sjawhar-legion-42.1",
      LEGION_MAX_RECURSION_DEPTH: "8",
      LEGION_STATE_DIR: stateDir,
      LEGION_CREDENTIAL_HELPER: "!/opt/legion/bun /opt/legion/cli/index.ts credential",
      GIT_CONFIG_COUNT: "0",
      GIT_TERMINAL_PROMPT: "0",
      PATH: "/full/bin:/usr/bin",
    });
  });
  it("gives collision-prone issue paths distinct escaped cosmetic window names", async () => {
    const stateDir = await temporaryDir();
    const left = formatIssueKey("example", "org-legion-smoke", 1);
    const right = formatIssueKey("example-org", "legion-smoke", 1);
    const { manager: processes, commands } = manager(newLegionState("omp", 2), {
      config: config(stateDir, { admissionCap: 2 }),
    });

    await processes.spawnRoot(left);
    await processes.spawnRoot(right);

    const names = commands
      .filter((command) => command[0] === "tmux" && command.includes("-n"))
      .map((command) => command[command.indexOf("-n") + 1]);
    expect(names).toEqual(["example__org_hlegion_hsmoke-1", "example_horg__legion_hsmoke-1"]);
  });

  it("caps an escaped cosmetic window name", async () => {
    const stateDir = await temporaryDir();
    const issue = formatIssueKey("a".repeat(200), "b".repeat(200), 1);
    const { manager: processes, commands } = manager(newLegionState("omp", 1), {
      config: config(stateDir),
    });

    await processes.spawnRoot(issue);

    const window = commands.find((command) => command[0] === "tmux" && command.includes("-n"));
    expect(window?.[window.indexOf("-n") + 1]).toHaveLength(160);
  });

  it("keeps the worker socket path under the Unix socket length limit for a very long issue key", async () => {
    const stateDir = await temporaryDir();
    const issue = formatIssueKey("a".repeat(200), "b".repeat(200), 1);
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

  it("kills and clears a dead tree's tmux locator", async () => {
    const state = newLegionState("omp", 1);
    tree(state);
    if (!state.trees[root]?.locator) throw new Error("test root is missing a locator");
    state.admission.active.push(root);
    const { manager: processes, commands } = manager(state);

    await processes.markProcessDead(root);

    expect(state.trees[root]?.locator).toBeUndefined();
    expect(commands).toContainEqual(["tmux", "kill-window", "-t", "@42"]);
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

    const workspaceDir = path.join(stateDir, "workspaces", "sjawhar", "legion", "issue-42");
    const controllerDir = path.join(stateDir, "controller");
    const extensionDir = path.resolve(import.meta.dir, "../../../../pi-envoy");
    const entrypoint = path.resolve(import.meta.dir, "../../cli/index.ts");
    const socketPath = path.join(stateDir, "workers", "42-architect-edb483d7.sock");
    const controllerSocketPath = path.join(stateDir, "workers", "controller.sock");
    const windows = commands.filter((command) => command[1] === "new-window");
    expect(windows.map((command) => command.at(-1))).toEqual([
      `cd ${workspaceDir} && ${process.execPath} ${entrypoint} worker-shim --socket ${socketPath} -- ${ompInvocation} --mode rpc --append-system-prompt "$(cat ${extensionDir}/roles/architect-root.md)" --append-system-prompt '${addressingFragment("omp", root, root, "architect").replaceAll("'", "'\\''")}'`,
      `cd ${controllerDir} && ${process.execPath} ${entrypoint} worker-shim --socket ${controllerSocketPath} -- ${ompInvocation} --mode rpc --append-system-prompt "$(cat ${extensionDir}/roles/controller-root.md)"`,
    ]);
    expect(windows.map((command) => command.includes(`PATH=${panePath}`))).toEqual([true, true]);
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
      heldEvents: [],
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
      heldEvents: [],
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
      heldEvents: [],
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
      heldEvents: [],
    };
    state.admission.active.push(root);
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
      heldEvents: [],
    };
    state.admission.active.push(root);
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
    const closed = formatIssueKey("sjawhar", "legion", 45);
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
      heldEvents: [],
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
      heldEvents: [],
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

    const workspace = path.join(stateDir, "workspaces", "sjawhar", "legion", "issue-42");
    const extension = path.resolve(import.meta.dir, "../../../../pi-envoy");
    const entrypoint = path.resolve(import.meta.dir, "../../cli/index.ts");
    const socketPath = path.join(stateDir, "workers", "42-architect-edb483d7.sock");
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

    const workspace = path.join(stateDir, "workspaces", "sjawhar", "legion", "issue-42");
    const extension = path.resolve(import.meta.dir, "../../../../pi-envoy");
    const entrypoint = path.resolve(import.meta.dir, "../../cli/index.ts");
    const socketPath = path.join(stateDir, "workers", "42-architect-edb483d7.sock");
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
      state: "closed",
      parent: root,
      children: [],
      released: true,
      labels: [],
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
    expect(commands).toContainEqual(["tmux", "kill-window", "-t", "@42"]);
  });

  it("awaits a promoted queued tree's full spawn attempt before beginLinger resolves", async () => {
    const stateDir = await temporaryDir();
    const state = newLegionState("omp", 1);
    tree(state);
    state.admission.active.push(root);
    state.admission.queue.push(child);
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
  it("kills every issue window in the tree when closing it, not just the root's own", async () => {
    const state = newLegionState("omp", 1);
    tree(state);
    state.issues[child] = {
      key: child,
      title: "Child",
      state: "open",
      parent: root,
      children: [],
      released: true,
      labels: [],
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
    const { manager: processes, commands } = manager(state);

    await processes.closeTree(root);

    expect(commands).toContainEqual(["tmux", "kill-window", "-t", "@42"]);
    expect(commands).toContainEqual(["tmux", "kill-window", "-t", "@99"]);
    expect(state.roles[roleToken("omp", child, "implementer")]).toBeUndefined();
    expect(state.trees[root].locator).toBeUndefined();
  });

  it("requests control directives on the sanitized tree generation topic", async () => {
    const state = newLegionState("omp", 1);
    tree(state, root, 3);
    const { manager: processes, controlRequests, publications } = manager(state);
    const directive: ControlDirective = { type: "shutdown" };

    await processes.controlDirective(root, directive);

    expect(controlRequests).toEqual([
      {
        subject: "legion.ctl.sjawhar-legion-42.3",
        json: '{"type":"shutdown"}',
      },
    ]);
    expect(publications).toEqual([]);
  });

  it("publishes post-backing events immediately after worker catch-up", async () => {
    const state = newLegionState("omp", 1);
    tree(state);
    state.issues[child] = {
      key: child,
      title: "Child",
      state: "open",
      parent: root,
      children: [],
      released: true,
      labels: [],
    };
    const original = exception(roleToken("omp", child, "implementer")).original;
    const { manager: processes, controlRequests, publications } = manager(state, { run: liveRun });
    await processes.registerRoleBacking(root, child, "implementer", "agent-worker");

    await processes.handleException(exception(roleToken("omp", child, "implementer"), original));

    expect(controlRequests).toHaveLength(1);
    expect(publications).toEqual([
      {
        subject: roleTopic(roleToken("omp", child, "implementer")),
        json: JSON.stringify({ type: "catchup-worker", unhandled: [] }),
      },
      { subject: original.topic, json: original.payload },
    ]);
    expect(state.trees[root]?.heldEvents).toEqual([]);
  });
  it("drains released child holds in order exactly once when role backing registers", async () => {
    const state = newLegionState("omp", 1);
    tree(state);
    state.issues[child] = {
      key: child,
      title: "Child",
      state: "open",
      parent: root,
      children: [],
      released: true,
      labels: [],
    };
    const token = roleToken("omp", child, "implementer");
    const first = {
      topic: roleTopic(token),
      payload: '{"sequence":1}',
      eventId: "evt-first",
    };
    const second = {
      topic: roleTopic(token),
      payload: '{"sequence":2}',
      eventId: "evt-second",
    };
    const { manager: processes, publications } = manager(state);

    await processes.handleException(exception(token, first));
    await processes.handleException(exception(token, second));
    await processes.registerRoleBacking(root, child, "implementer", "agent-worker");
    await processes.registerRoleBacking(root, child, "implementer", "agent-worker");

    expect(publications).toEqual([
      { subject: roleTopic(token), json: first.payload },
      { subject: roleTopic(token), json: second.payload },
    ]);
    expect(state.trees[root]?.heldEvents).toEqual([]);
  });
  it("keeps unreleased child holds until the wave release trigger", async () => {
    const state = newLegionState("omp", 1);
    tree(state);
    state.issues[child] = {
      key: child,
      title: "Child",
      state: "open",
      parent: root,
      children: [],
      released: false,
      labels: [],
    };
    const token = roleToken("omp", child, "implementer");
    const original = {
      topic: roleTopic(token),
      payload: '{"sequence":1}',
      eventId: "evt-unreleased",
    };
    const { manager: processes, publications } = manager(state);

    await processes.handleException(exception(token, original));
    await processes.registerRoleBacking(root, child, "implementer", "agent-worker");

    expect(publications).toEqual([]);
    expect(state.trees[root]?.heldEvents).toEqual([
      expect.objectContaining({
        eventId: original.eventId,
        role: "implementer",
      }),
    ]);
  });
  it("sends worker catch-up before the original event after a successful worker revival", async () => {
    const state = newLegionState("omp", 1);
    tree(state);
    state.issues[child] = {
      key: child,
      title: "Child",
      state: "open",
      parent: root,
      children: [],
      released: true,
      labels: [],
    };
    const role: LegionRole = "implementer";
    const token = roleToken("omp", child, role);
    state.roles[token] = { issue: child, role, agentId: "agent-worker" };
    const original = exception(token).original;
    const { manager: processes, publications } = manager(state, {
      run: async (command) => {
        if (command[0] === "gh") return { stdout: "[]", exitCode: 0 };
        return liveRun(command);
      },
    });

    await processes.handleException(exception(token, original));

    expect(publications).toEqual([
      {
        subject: roleTopic(token),
        json: JSON.stringify({ type: "catchup-worker", unhandled: [] }),
      },
      { subject: original.topic, json: original.payload },
    ]);
  });

  it("routes a directive nack to the controller without redelivering its exception", async () => {
    const state = newLegionState("omp", 1);
    tree(state);
    state.issues[child] = {
      key: child,
      title: "Child",
      state: "open",
      parent: root,
      children: [],
      released: true,
      labels: [],
    };
    state.roles[roleToken("omp", child, "implementer")] = {
      issue: child,
      role: "implementer",
      agentId: "agent-worker",
    };
    const original = exception(roleToken("omp", child, "implementer")).original;
    const { manager: processes, publications } = manager(state, {
      run: liveRun,
      natsRequest: async () =>
        JSON.stringify({ type: "nack", error: "worker transcript is missing" }),
    });

    await processes.handleException(exception(roleToken("omp", child, "implementer"), original));

    expect(publications).toEqual([
      {
        subject: `notifications.role.${controllerToken("omp")}`,
        json: JSON.stringify({
          type: "revive-failed",
          issue: child,
          role: "implementer",
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

  it("persists registered role backing so a restarted manager can revive its durable agent target", async () => {
    const stateDir = await temporaryDir();
    const stateFile = path.join(stateDir, "state.json");
    const state = newLegionState("omp", 1);
    tree(state);
    state.issues[child] = {
      key: child,
      title: "Child",
      state: "open",
      parent: root,
      children: [],
      released: true,
      labels: [],
    };
    const first = manager(state, {
      saveState: () => saveState(stateFile, state),
    }).manager;

    await first.registerRoleBacking(root, child, "architect", "agt-restarted");

    const restarted = await loadState(stateFile, { project: "omp", cap: 1 });
    const { manager: processes, publications } = manager(restarted, {
      run: liveRun,
    });
    await processes.handleException(exception(roleToken("omp", child, "architect")));

    const original = exception(roleToken("omp", child, "architect")).original;
    expect(publications).toEqual([
      {
        subject: roleTopic(roleToken("omp", child, "architect")),
        json: JSON.stringify({ type: "catchup-worker", unhandled: [] }),
      },
      { subject: original.topic, json: original.payload },
    ]);
  });

  it("marks an exited tree dead, releases its admission slot, and preserves held events", async () => {
    const state = newLegionState("omp", 1);
    tree(state);
    state.admission.active.push(root);
    state.trees[root].heldEvents.push({
      role: "implementer",
      payloadJson: '{"kind":"pending"}',
      heldAt: "2026-08-24T00:00:00.000Z",
      eventId: "evt-held",
    });
    let saves = 0;
    const { manager: processes } = manager(state, {
      saveState: async () => {
        saves += 1;
      },
    });

    await processes.markProcessDead(root);

    expect(state.trees[root]).toMatchObject({
      status: "dead",
      heldEvents: [
        {
          role: "implementer",
          payloadJson: '{"kind":"pending"}',
          heldAt: "2026-08-24T00:00:00.000Z",
          eventId: "evt-held",
        },
      ],
    });
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

  it("reclaims a live root architect directly instead of holding its exception", async () => {
    const state = newLegionState("omp", 1);
    tree(state);
    const { manager: processes, publications } = manager(state, {
      run: liveRun,
    });

    await processes.handleException(exception(roleToken("omp", root, "architect")));

    const original = exception(roleToken("omp", root, "architect")).original;
    expect(publications).toEqual([{ subject: original.topic, json: original.payload }]);
    expect(state.trees[root].heldEvents).toEqual([]);
  });

  it("resurrects a dead root architect instead of holding the root event", async () => {
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

    expect(state.trees[root]).toMatchObject({ generation: 2, heldEvents: [] });
  });
  it("persists a dead worker's original delivery before resurrecting its root", async () => {
    const stateDir = await temporaryDir();
    const sessionFile = path.join(stateDir, "architect-session.json");
    await writeFile(sessionFile, "{}", "utf8");
    const state = newLegionState("omp", 1);
    tree(state);
    const rootLocator = state.trees[root].locator;
    if (!rootLocator) throw new Error("test root is missing a locator");
    state.trees[root].locator = { ...rootLocator, ompSessionFile: sessionFile };
    state.issues[child] = {
      key: child,
      title: "Child",
      state: "open",
      parent: root,
      children: [],
      released: true,
      labels: [],
    };
    const role: LegionRole = "implementer";
    const token = roleToken("omp", child, role);
    state.roles[token] = { issue: child, role, agentId: "agt-worker" };
    const original = exception(token).original;
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

    await processes.handleException(exception(token, original));

    const recoveredTree = state.trees[root] as unknown as {
      recoveryEvents?: Array<{
        issue: IssueKey;
        role: LegionRole;
        original: unknown;
      }>;
    };
    expect(recoveredTree.recoveryEvents).toEqual([{ issue: child, role, original }]);
  });
  it("delivers worker catch-up and the original event once the resurrected root is ready", async () => {
    const stateDir = await temporaryDir();
    const sessionFile = path.join(stateDir, "architect-session.json");
    await writeFile(sessionFile, "{}", "utf8");
    const state = newLegionState("omp", 1);
    tree(state);
    const rootLocator = state.trees[root].locator;
    if (!rootLocator) throw new Error("test root is missing a locator");
    state.trees[root].locator = { ...rootLocator, ompSessionFile: sessionFile };
    state.issues[child] = {
      key: child,
      title: "Child",
      state: "open",
      parent: root,
      children: [],
      released: true,
      labels: [],
    };
    const role: LegionRole = "implementer";
    const token = roleToken("omp", child, role);
    state.roles[token] = { issue: child, role, agentId: "agt-worker" };
    const original = exception(token).original;
    let launched = false;
    const { manager: processes, publications } = manager(state, {
      config: config(stateDir),
      run: async (command) => {
        if (command[1] === "list-windows") return { stdout: "", exitCode: 1 };
        if (command[0] === "gh") return { stdout: "[]", exitCode: 0 };
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

    await processes.handleException(exception(token, original));
    const locator = state.trees[root].locator;
    if (!locator) throw new Error("resurrected root is missing its locator");
    state.trees[root].locator = {
      ...locator,
      ompSessionFile: "/state/root-session.json",
    };
    await processes.markTreeReady(root);

    expect(publications).toEqual([
      {
        subject: roleTopic(token),
        json: JSON.stringify({ type: "catchup-worker", unhandled: [] }),
      },
      { subject: original.topic, json: original.payload },
    ]);
    expect(state.trees[root].recoveryEvents).toEqual([]);
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

  it("holds an exception until an unbacked worker exists", async () => {
    const state = newLegionState("omp", 1);
    tree(state);
    state.issues[child] = {
      key: child,
      title: "Child",
      state: "open",
      parent: root,
      children: [],
      released: true,
      labels: [],
    };
    const { manager: processes } = manager(state);

    await processes.handleException(exception(roleToken("omp", child, "implementer")));

    expect(state.trees[root].heldEvents).toEqual([
      {
        role: "implementer",
        payloadJson: '{"body":"retry"}',
        heldAt: "2026-08-24T00:00:00.000Z",
        eventId: "evt-1",
      },
    ]);
  });

  it("revives a live backed sub-architect with its agent target and root session file", async () => {
    const state = newLegionState("omp", 1);
    tree(state);
    state.issues[child] = {
      key: child,
      title: "Child",
      state: "open",
      parent: root,
      children: [],
      released: true,
      labels: [],
    };
    const role: LegionRole = "architect";
    state.roles[roleToken("omp", child, role)] = {
      issue: child,
      role,
      agentId: "agt-sub-architect",
    } as LegionState["roles"][string];
    const { manager: processes, publications } = manager(state, {
      run: liveRun,
    });

    await processes.handleException(exception(roleToken("omp", child, role)));

    const original = exception(roleToken("omp", child, role)).original;
    expect(publications).toEqual([
      {
        subject: roleTopic(roleToken("omp", child, role)),
        json: JSON.stringify({ type: "catchup-worker", unhandled: [] }),
      },
      { subject: original.topic, json: original.payload },
    ]);
  });

  it("resurrects a dead backed worker and reports a revival nack to the controller", async () => {
    const stateDir = await temporaryDir();
    const sessionFile = path.join(stateDir, "architect-session.json");
    await writeFile(sessionFile, "{}", "utf8");
    const state = newLegionState("omp", 1);
    tree(state);
    const rootLocator = state.trees[root].locator;
    if (!rootLocator) throw new Error("test root is missing a locator");
    state.trees[root].locator = { ...rootLocator, ompSessionFile: sessionFile };
    state.issues[child] = {
      key: child,
      title: "Child",
      state: "open",
      parent: root,
      children: [],
      released: true,
      labels: [],
    };
    state.roles[roleToken("omp", child, "implementer")] = {
      issue: child,
      role: "implementer",
      agentId: "agt-worker",
    } as LegionState["roles"][string];
    let launched = false;
    const { manager: processes, publications } = manager(state, {
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

    await processes.handleException(exception(roleToken("omp", child, "implementer")));
    await processes.handleException({
      ...exception(roleToken("omp", child, "implementer")),
      nack: { type: "revive-worker", issue: child, role: "implementer" },
    });

    expect(state.trees[root].generation).toBe(2);
    expect(publications).toContainEqual({
      subject: `notifications.role.${controllerToken("omp")}`,
      json: JSON.stringify({
        type: "revive-failed",
        issue: child,
        role: "implementer",
      }),
    });
  });

  // Requires a real tmux installation to prove window IDs survive cosmetic-name collisions.
  it.skipIf(process.env.LEGION_TMUX_LIVE !== "1")(
    "probes its live root through its window id when a duplicate cosmetic name exists",
    async () => {
      const stateDir = await temporaryDir();
      const project = `duplicate${Date.now()}`;
      const state = newLegionState(project, 1);
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
          "sjawhar__legion-42"
        );
      } finally {
        await commandRunner(["tmux", "kill-session", "-t", session]);
      }
    }
  );

  it("spawns a worker's first pane as a new window with the full worker env and worker-shim command", async () => {
    const stateDir = await temporaryDir();
    const workspace = path.join(stateDir, "workspaces", "sjawhar", "legion", "issue-42");
    await mkdir(workspace, { recursive: true });
    const state = newLegionState("omp", 1);
    state.issues[root] = {
      key: root,
      title: "Root",
      state: "open",
      children: [],
      released: true,
      labels: [],
    };
    state.trees[root] = {
      root,
      generation: 1,
      status: "active",
      launchFailures: 0,
      heldEvents: [],
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
    expect(windowCommand[windowCommand.indexOf("-n") + 1]).toBe("sjawhar__legion-42");
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
      `cd ${workspace} && ${process.execPath} ${path.resolve(import.meta.dir, "../../cli/index.ts")} worker-shim --socket ${path.join(stateDir, "workers", "42-tester-edb483d7.sock")} -- /opt/oh-my-pi/18.0.3/omp --mode rpc --append-system-prompt "$(cat ${promptPath})" --append-system-prompt '${addressingFragment("omp", root, root, "tester").replaceAll("'", "'\\''")}'`
    );
    const claim = managedState.roles[roleToken("omp", root, "tester")];
    if (!claim || !("issue" in claim)) throw new Error("worker claim was not recorded");
    expect(claim.generation).toBe(1);
    expect(claim.pendingAssignment).toBe("verify #41");
    expect(claim.locator).toMatchObject({
      tmuxSession: "legion-omp",
      tmuxWindowId: "@99",
      tmuxPaneId: "%201",
      socketPath: path.join(stateDir, "workers", "42-tester-edb483d7.sock"),
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
      state: "open",
      children: [],
      released: true,
      labels: [],
    };
    state.trees[root] = {
      root,
      generation: 1,
      status: "active",
      launchFailures: 0,
      heldEvents: [],
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
      state: "open",
      children: [],
      released: true,
      labels: [],
    };
    state.trees[root] = {
      root,
      generation: 1,
      status: "active",
      launchFailures: 0,
      heldEvents: [],
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
      state: "open",
      children: [],
      released: true,
      labels: [],
    };
    state.trees[root] = {
      root,
      generation: 1,
      status: "active",
      launchFailures: 0,
      heldEvents: [],
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

  it("rewrites every claim's stale window id once a dead recorded window falls back to a fresh one", async () => {
    const stateDir = await temporaryDir();
    const workspace = path.join(stateDir, "workspaces", "sjawhar", "legion", "issue-42");
    await mkdir(workspace, { recursive: true });
    const state = newLegionState("omp", 1);
    state.issues[root] = {
      key: root,
      title: "Root",
      state: "open",
      children: [],
      released: true,
      labels: [],
    };
    state.trees[root] = {
      root,
      generation: 1,
      status: "active",
      launchFailures: 0,
      heldEvents: [],
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
      state: "open",
      children: [],
      released: true,
      labels: [],
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
      state: "open",
      children: [],
      released: true,
      labels: [],
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
      state: "open",
      children: [],
      released: true,
      labels: [],
    };
    state.trees[root] = {
      root,
      generation: 1,
      status: "active",
      launchFailures: 0,
      heldEvents: [],
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
      saveState: async () => {
        const testerClaim = managedState.roles[testerToken];
        if (testerClaim && "issue" in testerClaim && testerClaim.locator) resolvePromoted?.();
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

    occupierClient.emitRunState("idle");
    await promoted;

    const promotedClaim = managedState.roles[testerToken];
    if (!promotedClaim || !("issue" in promotedClaim)) throw new Error("tester claim disappeared");
    expect(managedState.workerAdmission.queue).toEqual([]);
    expect(promotedClaim.resumeSessionFile).toBeUndefined();
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
      state: "open",
      children: [],
      released: true,
      labels: [],
    };
    state.trees[root] = {
      root,
      generation: 1,
      status: "active",
      launchFailures: 0,
      heldEvents: [],
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
      state: "open",
      children: [],
      released: true,
      labels: [],
    };
    state.trees[root] = {
      root,
      generation: 1,
      status: "active",
      launchFailures: 0,
      heldEvents: [],
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

  it("passes a respawned claim's existing sessionId as the worker boot token's expected session", async () => {
    const stateDir = await temporaryDir();
    const workspace = path.join(stateDir, "workspaces", "sjawhar", "legion", "issue-42");
    await mkdir(workspace, { recursive: true });
    const state = newLegionState("omp", 1);
    state.issues[root] = {
      key: root,
      title: "Root",
      state: "open",
      children: [],
      released: true,
      labels: [],
    };
    state.trees[root] = {
      root,
      generation: 1,
      status: "active",
      launchFailures: 0,
      heldEvents: [],
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
      state: "open",
      children: [child],
      released: true,
      labels: [],
    };
    state.issues[child] = {
      key: child,
      title: "Child",
      parent: root,
      state: "open",
      children: [],
      released: true,
      labels: [],
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

  it("promotes the queued worker once the running one dies, same as going idle", async () => {
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

    const plannerClaim = managedState.roles[plannerToken];
    if (!plannerClaim || !("issue" in plannerClaim)) throw new Error("planner claim missing");
    plannerClaim.sessionId = "ses_planner";
    await processes.workerReady(root, "planner", "ses_planner", plannerClaim.generation ?? 1);

    clients[0]?.close();
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

  it("re-evaluates a persisted running-worker queue against a raised cap across a restart", async () => {
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
    const publications: Array<{ subject: string; json: string }> = [];
    let resolvePromoted: (() => void) | undefined;
    const promoted = new Promise<void>((resolve) => {
      resolvePromoted = resolve;
    });
    const architectTopic = roleTopic(roleToken("omp", root, "architect"));
    const { manager: processes, state: managedState } = manager(state, {
      config: config(stateDir, { workerCap: 2 }),
      natsPublish: (subject, json) => {
        publications.push({ subject, json });
        if (subject === architectTopic && json.includes("worker-started")) resolvePromoted?.();
      },
    });

    processes.reconcileWorkerAdmission();
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

  it("delivers a worker's pending assignment over its socket on worker/ready and clears it", async () => {
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
    const { manager: processes, state: managedState } = manager(state, {
      connectWorkerRpc: async () => client,
    });

    await processes.workerReady(root, "tester", "ses_tester", 1);

    expect(client.prompts).toEqual(["verify #41"]);
    const claim = managedState.roles[token];
    if (!claim || !("issue" in claim)) throw new Error("worker claim disappeared");
    expect(claim.pendingAssignment).toBeUndefined();
    // Same fix as the resumed-live-socket branch of spawnWorker: a delivered pending assignment
    // must re-register as the issue's active phase, or the worker's eventual `handoff complete`
    // 409s against a phase this delivery path never restored.
    expect(managedState.phases[root]).toEqual({ phase: "tester", sessionId: "ses_tester" });
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

  it("passes both DISPATCH_URL and the transitional DISPATCH_MCP_URL alias to a spawned phase worker", async () => {
    const stateDir = await temporaryDir();
    const workspace = path.join(stateDir, "workspaces", "sjawhar", "legion", "issue-42");
    await mkdir(workspace, { recursive: true });
    const state = newLegionState("omp", 1);
    state.trees[root] = {
      root,
      generation: 1,
      status: "active",
      launchFailures: 0,
      heldEvents: [],
    };
    const { manager: processes, commands } = manager(state, {
      config: config(stateDir, { dispatchUrl: "http://127.0.0.1:18766" }),
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
    expect(environment.DISPATCH_MCP_URL).toBe("http://127.0.0.1:18766/mcp");
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

  it("swallows a controller shim connect failure on ready, so held-event replay is never blocked by it", async () => {
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
});

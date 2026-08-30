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
  type ControlDirective,
  type ExceptionInfo,
  ProcessManager,
  type ProcessManagerDeps,
} from "../processes";

const root = formatIssueKey("sjawhar", "legion", 42);
const child = formatIssueKey("sjawhar", "legion", 43);
const grandchild = formatIssueKey("sjawhar", "legion", 44);
const tempDirs: string[] = [];

async function temporaryDir(): Promise<string> {
  const directory = await mkdtemp(path.join(os.tmpdir(), "legion-processes-"));
  tempDirs.push(directory);
  return directory;
}

function config(stateDir: string, overrides: Partial<DaemonConfig> = {}): DaemonConfig {
  return {
    project: "omp",
    legionId: "sjawhar/1",
    port: 13999,
    envoyUrl: "http://127.0.0.1:9020",
    natsUrls: ["nats://127.0.0.1:4222"],
    dispatchUrl: "http://127.0.0.1:13380",
    dispatchBearer: "dispatch-bearer",
    ompInvocation: "mise x github:sjawhar/oh-my-pi@18.0.3-sami.20260824-002841 -- omp",
    boardProjectIds: [],
    appLogins: [],
    admissionCap: 1,
    workerBudget: 5,
    maxRecursionDepth: 8,
    lingerHours: 2,
    ciQuietMs: 30_000,
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
  const commandRunner =
    requestedRun ??
    (async (command: string[]) => {
      commands.push(command);
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
        if (command[1] === "new-window" && ++windows === 2) completeSpawns?.();
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
      config: config(stateDir),
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
        return command[1] === "has-session"
          ? { stdout: "", exitCode: 1 }
          : { stdout: "", exitCode: 0 };
      },
    });

    await processes.spawnRoot(root);

    expect(await readFile(path.join(workspace, ".omp", "config.yml"), "utf8")).toBe(
      `task:\n  maxRecursionDepth: 8\nextensions:\n  - ${path.resolve(import.meta.dir, "../../../../envoy-omp-extension")}\n`
    );
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
        "#{window_id}",
        "-t",
        "legion-omp",
        "-n",
        "sjawhar__legion-42",
        "-e",
        "LEGION_TREE=sjawhar/legion#42",
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
        "LEGION_WORKER_BUDGET=5",
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
        `cd ${workspace} && /opt/oh-my-pi/18.0.3/omp --extension ${path.resolve(import.meta.dir, "../../../../envoy-omp-extension")} --append-system-prompt "$(cat ${path.resolve(import.meta.dir, "../../../../envoy-omp-extension")}/agents/architect-root.md)"`,
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
      LEGION_CONTROLLER_SECRET: "controller-secret",
      LEGION_DAEMON_URL: "http://127.0.0.1:13999",
      LEGION_PROJECT: "omp",
      ENVOY_NATS_URL: "nats://127.0.0.1:4222",
      ENVOY_URL: "http://127.0.0.1:9020",
      PATH: "/full/bin:/usr/bin",
    });
    expect(tmuxWindowEnvironment(rootWindow)).toEqual({
      LEGION_TREE: root,
      LEGION_ROOT_WORKSPACE: path.join(stateDir, "workspaces", "sjawhar", "legion", "issue-42"),
      LEGION_GENERATION: "1",
      LEGION_BOOT_TOKEN: "boot-token",
      LEGION_DAEMON_URL: "http://127.0.0.1:13999",
      LEGION_PROJECT: "omp",
      ENVOY_NATS_URL: "nats://127.0.0.1:4222",
      ENVOY_URL: "http://127.0.0.1:9020",
      LEGION_CONTROL_SUBJECT: "legion.ctl.sjawhar-legion-42.1",
      LEGION_WORKER_BUDGET: "5",
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
    const left = formatIssueKey("trajectory-labs", "pbc-legion-smoke", 1);
    const right = formatIssueKey("trajectory-labs-pbc", "legion-smoke", 1);
    const { manager: processes, commands } = manager(newLegionState("omp", 2), {
      config: config(stateDir, { admissionCap: 2 }),
    });

    await processes.spawnRoot(left);
    await processes.spawnRoot(right);

    const names = commands
      .filter((command) => command[0] === "tmux" && command.includes("-n"))
      .map((command) => command[command.indexOf("-n") + 1]);
    expect(names).toEqual([
      "trajectory_hlabs__pbc_hlegion_hsmoke-1",
      "trajectory_hlabs_hpbc__legion_hsmoke-1",
    ]);
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

  it("records a new tmux window id and probes that id rather than its cosmetic name", async () => {
    const stateDir = await temporaryDir();
    const commands: string[][] = [];
    const { manager: processes, state } = manager(newLegionState("omp", 1), {
      config: config(stateDir),
      run: async (command) => {
        commands.push(command);
        if (command[1] === "has-session") return { stdout: "", exitCode: 0 };
        if (command[1] === "new-window") return { stdout: "@314\n", exitCode: 0 };
        if (command[1] === "list-panes" && command.includes("@314")) {
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
    });
    expect(await processes.probe(root)).toBe("alive");
    expect(commands).toContainEqual(["tmux", "list-panes", "-t", "@314", "-F", "#{pane_pid}"]);
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

  it("passes the packaged extension root to OMP discovery for root and controller windows", async () => {
    const stateDir = await temporaryDir();
    const ompInvocation = "/opt/oh-my-pi/18.0.3/omp";
    const panePath = "/full/bin:/usr/bin";
    const { manager: processes, commands } = manager(newLegionState("omp", 1), {
      config: config(stateDir),
      ompInvocation,
      panePath,
      run: async (command) => {
        commands.push(command);
        return { stdout: "", exitCode: 0 };
      },
    });

    await processes.spawnRoot(root);
    await processes.ensureController();

    const workspaceDir = path.join(stateDir, "workspaces", "sjawhar", "legion", "issue-42");
    const controllerDir = path.join(stateDir, "controller");
    const extensionDir = path.resolve(import.meta.dir, "../../../../envoy-omp-extension");
    const windows = commands.filter((command) => command[1] === "new-window");
    expect(windows.map((command) => command.at(-1))).toEqual([
      `cd ${workspaceDir} && ${ompInvocation} --extension ${extensionDir} --append-system-prompt "$(cat ${extensionDir}/agents/architect-root.md)"`,
      `cd ${controllerDir} && ${ompInvocation} --extension ${extensionDir} --append-system-prompt "$(cat ${extensionDir}/agents/controller-root.md)"`,
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

    processes.reconcileAdmission();

    expect(state.admission).toEqual({ cap: 2, active: [root, child], queue: [] });
    expect(state.trees[root]?.status).toBe("active");
    expect(state.trees[child]?.status).toBe("active");
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

    processes.reconcileAdmission();

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

    processes.reconcileAdmission();

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

    processes.reconcileAdmission();

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

    processes.reconcileAdmission();
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

    processes.reconcileAdmission();

    expect(state.admission).toEqual({
      cap: 5,
      active: [root],
      queue: [root, child, grandchild, closed],
    });
    expect(commands).toEqual([]);
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
    const state = newLegionState("omp", 1);
    tree(state);
    let windows = 0;
    const { manager: processes } = manager(state, {
      config: config(stateDir),
      run: async (command) => {
        if (command[1] === "list-windows") return { stdout: "", exitCode: 1 };
        if (command[1] === "has-session") return { stdout: "", exitCode: 0 };
        if (command[1] === "new-window") windows += 1;
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
    const extension = path.resolve(import.meta.dir, "../../../../envoy-omp-extension");
    const launch = commands.find((command) => command[0] === "tmux" && command[1] === "new-window");
    expect(launch?.at(-1)).toBe(
      `cd ${workspace} && /opt/oh-my-pi/18.0.3/omp --resume=${sessionFile} --extension ${extension} --append-system-prompt "$(cat ${extension}/agents/architect-root.md)"`
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
    const extension = path.resolve(import.meta.dir, "../../../../envoy-omp-extension");
    const launch = commands.find((command) => command[0] === "tmux" && command[1] === "new-window");
    expect(launch?.at(-1)).toBe(
      `cd ${workspace} && /opt/oh-my-pi/18.0.3/omp --extension ${extension} --append-system-prompt "$(cat ${extension}/agents/architect-root.md)"`
    );
  });

  it("logs and starts fresh when a resurrected root's recorded OMP session is missing", async () => {
    const stateDir = await temporaryDir();
    const sessionFile = path.join(stateDir, "missing-architect-session.json");
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
      await processes.resurrect(root);
      expect(log).toHaveBeenCalledWith(
        `[legion] resurrecting ${root} with a fresh OMP session; recorded session file is missing: ${sessionFile}`
      );
    } finally {
      log.mockRestore();
    }

    const workspace = path.join(stateDir, "workspaces", "sjawhar", "legion", "issue-42");
    const extension = path.resolve(import.meta.dir, "../../../../envoy-omp-extension");
    const launch = commands.find((command) => command[0] === "tmux" && command[1] === "new-window");
    expect(launch?.at(-1)).toBe(
      `cd ${workspace} && /opt/oh-my-pi/18.0.3/omp --extension ${extension} --append-system-prompt "$(cat ${extension}/agents/architect-root.md)"`
    );
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

    processes.beginLinger(root);

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
        if (command[1] === "new-window") controllerSpawned = true;
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
        if (command[1] === "new-window") controllerLive = true;
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
        if (command[1] === "new-window") controllerSpawned = true;
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
    const state = newLegionState("omp", 1);
    tree(state);
    const { manager: processes } = manager(state, {
      config: config(stateDir),
      run: async (command) => {
        if (command[1] === "list-windows") return { stdout: "", exitCode: 1 };
        return { stdout: "", exitCode: 0 };
      },
    });

    await processes.handleException(exception(roleToken("omp", root, "architect")));

    expect(state.trees[root]).toMatchObject({ generation: 2, heldEvents: [] });
  });
  it("persists a dead worker's original delivery before resurrecting its root", async () => {
    const stateDir = await temporaryDir();
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
    state.roles[token] = { issue: child, role, agentId: "agt-worker" };
    const original = exception(token).original;
    const { manager: processes } = manager(state, {
      config: config(stateDir),
      run: async (command) =>
        command[1] === "list-windows" ? { stdout: "", exitCode: 1 } : { stdout: "", exitCode: 0 },
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
    state.roles[token] = { issue: child, role, agentId: "agt-worker" };
    const original = exception(token).original;
    const { manager: processes, publications } = manager(state, {
      config: config(stateDir),
      run: async (command) => {
        if (command[1] === "list-windows") return { stdout: "", exitCode: 1 };
        if (command[0] === "gh") return { stdout: "[]", exitCode: 0 };
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
      agentId: "agt-worker",
    } as LegionState["roles"][string];
    const { manager: processes, publications } = manager(state, {
      config: config(stateDir),
      run: async (command) => {
        if (command[1] === "list-windows") return { stdout: "", exitCode: 1 };
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
});

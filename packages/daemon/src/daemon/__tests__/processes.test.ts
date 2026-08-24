import { afterAll, describe, expect, it } from "bun:test";
import { mkdtemp, readFile, rm } from "node:fs/promises";
import os from "node:os";
import path from "node:path";
import {
  controllerToken,
  formatIssueKey,
  type IssueKey,
  type LegionRole,
  roleToken,
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
      tmuxWindow: "sjawhar-legion-42",
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
  options: Partial<ProcessManagerDeps> = {}
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
  const deps: ProcessManagerDeps = {
    state,
    saveState: async () => {},
    config: config("/state"),
    run: async (command) => {
      commands.push(command);
      return { stdout: "", exitCode: 0 };
    },
    natsPublish: (subject, json) => publications.push({ subject, json }),
    natsRequest: async (subject, json) => {
      controlRequests.push({ subject, json });
      return JSON.stringify({ type: "ack" });
    },
    mintControllerCapability: async () => "controller-secret",
    mintBootToken: async () => "boot-token",
    statPrompt: async () => {},
    now: () => Date.parse("2026-08-24T00:00:00.000Z"),
    ...options,
  };
  return { manager: new ProcessManager(deps), state, commands, controlRequests, publications };
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

  it("writes the root extension config before executing the exact tmux spawn argv", async () => {
    const stateDir = await temporaryDir();
    const {
      manager: processes,
      state,
      commands,
    } = manager(newLegionState("omp", 1), {
      config: config(stateDir),
      run: async (command) => {
        commands.push(command);
        return command[1] === "has-session"
          ? { stdout: "", exitCode: 1 }
          : { stdout: "", exitCode: 0 };
      },
    });

    await processes.spawnRoot(root);

    const treeDir = path.join(stateDir, "trees", "sjawhar-legion-42");
    expect(await readFile(path.join(treeDir, ".omp", "config.yml"), "utf8")).toBe(
      `task:\n  maxRecursionDepth: 8\nextensions:\n  - ${path.resolve(import.meta.dir, "../../../../envoy-omp-extension")}\n`
    );
    expect(state.trees[root]).toMatchObject({ generation: 1, status: "active" });
    expect(commands).toEqual([
      ["tmux", "has-session", "-t", "legion-omp"],
      ["tmux", "new-session", "-d", "-s", "legion-omp"],
      [
        "tmux",
        "new-window",
        "-t",
        "legion-omp",
        "-n",
        "sjawhar-legion-42",
        "-e",
        "LEGION_TREE=sjawhar/legion#42",
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
        `cd ${treeDir} && omp --append-system-prompt "$(cat ${path.resolve(import.meta.dir, "../../../../envoy-omp-extension")}/agents/architect-root.md)"`,
      ],
    ]);
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
      expect(state.trees[root]).toMatchObject({ status: "queued", launchFailures: 1 });
      expect(state.trees[child]).toMatchObject({ status: "queued", launchFailures: 1 });
    } finally {
      console.error = originalConsoleError;
    }
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

    expect(state.trees[root]).toMatchObject({ status: "launch-failed", launchFailures: 3 });
    expect(state.admission).toEqual({ cap: 1, active: [], queue: [] });
    expect(publications).toEqual([
      {
        subject: `notifications.role.${controllerToken("omp")}`,
        json: JSON.stringify({ type: "launch-failed", issue: root, failures: 3 }),
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

    expect(state.trees[root]).toMatchObject({ status: "active", launchFailures: 0 });
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
    expect(state.trees[root]).toMatchObject({ status: "active", launchFailures: 1 });
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

  it("releases the admission slot at linger start, then shuts down and closes its recorded tmux tree", () => {
    const state = newLegionState("omp", 1);
    tree(state);
    state.admission.active.push(root);
    state.roles[roleToken("omp", root, "architect")] = { issue: root, role: "architect" };
    const { manager: processes, commands, publications } = manager(state);

    processes.beginLinger(root);

    expect(state.trees[root]).toMatchObject({
      status: "lingering",
      lingerUntil: "2026-08-24T02:00:00.000Z",
    });
    expect(state.admission).toEqual({ cap: 1, active: [], queue: [] });

    processes.expireLinger(root);

    expect(state.trees[root].status).toBe("closed");
    expect(state.roles[roleToken("omp", root, "architect")]).toBeUndefined();
    expect(publications).toEqual([]);
    expect(commands).toContainEqual(["tmux", "kill-window", "-t", "legion-omp:sjawhar-legion-42"]);
  });

  it("requests control directives on the sanitized tree generation topic", async () => {
    const state = newLegionState("omp", 1);
    tree(state, root, 3);
    const { manager: processes, controlRequests, publications } = manager(state);
    const directive: ControlDirective = { type: "shutdown" };

    await processes.controlDirective(root, directive);

    expect(controlRequests).toEqual([
      { subject: "legion.ctl.sjawhar-legion-42.3", json: '{"type":"shutdown"}' },
    ]);
    expect(publications).toEqual([]);
  });

  it("redelivers an exception exactly once after its control directive acknowledges", async () => {
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
    const { manager: processes, controlRequests, publications } = manager(state, { run: liveRun });

    await processes.handleException(exception(roleToken("omp", child, "implementer"), original));

    expect(controlRequests).toHaveLength(1);
    expect(publications).toEqual([{ subject: original.topic, json: original.payload }]);
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
      natsRequest: async () => JSON.stringify({ type: "nack", error: "worker transcript is missing" }),
    });

    await processes.handleException(exception(roleToken("omp", child, "implementer"), original));

    expect(publications).toEqual([
      {
        subject: `notifications.role.${controllerToken("omp")}`,
        json: JSON.stringify({ type: "revive-failed", issue: child, role: "implementer" }),
      },
    ]);
  });

  it("spawns one controller window and redelivers its held exception after the controller claim registers", async () => {
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
          return { stdout: controllerSpawned ? "controller\n" : "", exitCode: 0 };
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
    const original = {
      topic: "notifications.github.sjawhar.legion.issue.42.comment",
      payload: "{}",
      eventId: "evt-controller",
    };

    await processes.handleException(exception(controllerToken("omp"), original));
    state.roles[controllerToken("omp")] = {
      issue: root,
      role: "controller",
      sessionId: "ses-controller",
    };
    await processes.markControllerReady();

    expect(commands.filter((command) => command[1] === "new-window")).toHaveLength(1);
    expect(publications).toEqual([{ subject: original.topic, json: original.payload }]);
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

  it("keeps controller exceptions held when a stale controller claim survives replacement spawning", async () => {
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
          return { stdout: controllerSpawned ? "controller\n" : "", exitCode: 0 };
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
    const original = {
      topic: "notifications.github.sjawhar.legion.issue.42.comment",
      payload: '{"body":"controller retry"}',
      eventId: "evt-stale-controller",
    };

    await processes.handleException(exception(controllerToken("omp"), original));

    expect(publications).toEqual([]);
    state.roles[controllerToken("omp")].sessionId = "ses-replacement";
    await processes.markControllerReady();

    expect(publications).toEqual([{ subject: original.topic, json: original.payload }]);
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
    const { manager: processes, publications } = manager(restarted, { run: liveRun });
    await processes.handleException(exception(roleToken("omp", child, "architect")));

    const original = exception(roleToken("omp", child, "architect")).original;
    expect(publications).toEqual([{ subject: original.topic, json: original.payload }]);
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
    const { manager: processes, publications } = manager(state, { run: liveRun });

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
    const { manager: processes, publications } = manager(state, { run: liveRun });

    await processes.handleException(exception(roleToken("omp", child, role)));

    const original = exception(roleToken("omp", child, role)).original;
    expect(publications).toEqual([{ subject: original.topic, json: original.payload }]);
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
      json: JSON.stringify({ type: "revive-failed", issue: child, role: "implementer" }),
    });
  });

  it.skipIf(process.env.LEGION_TMUX_LIVE !== "1")(
    "probes a real tmux pane as alive, detects its death, and resurrects it once",
    async () => {
      const stateDir = await temporaryDir();
      const project = `smoke${Date.now()}`;
      const state = newLegionState(project, 1);
      const session = `legion-${project}`;
      const commandRunner = async (command: string[]) => {
        const actual =
          command[0] === "tmux" && command[1] === "new-window"
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
        expect(await processes.probe(root)).toBe("alive");
        await commandRunner(["tmux", "kill-pane", "-t", `${session}:sjawhar-legion-42`]);
        expect(await processes.probe(root)).toBe("dead");
        await Promise.all([processes.resurrect(root), processes.resurrect(root)]);
        expect(await processes.probe(root)).toBe("alive");
        expect((await commandRunner(["tmux", "list-windows", "-t", session])).stdout).toContain(
          "sjawhar-legion-42"
        );
      } finally {
        await commandRunner(["tmux", "kill-session", "-t", session]);
      }
    }
  );
});

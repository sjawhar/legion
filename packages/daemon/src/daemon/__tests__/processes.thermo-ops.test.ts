import { expect, it } from "bun:test";
import { mkdtemp, rm } from "node:fs/promises";
import os from "node:os";
import path from "node:path";
import type { DaemonConfig } from "../config";
import { type LegionState, newLegionState } from "../legion-state";
import { ProcessManager, type ProcessManagerDeps } from "../processes";
import { fakeDispatchClient } from "./ci-fixtures";

const now = Date.parse("2026-08-26T00:00:00.000Z");

function daemonConfig(stateDir: string): DaemonConfig {
  return {
    project: "omp",
    legionId: "sjawhar/1",
    port: 13999,
    envoyUrl: "http://127.0.0.1:9020",
    natsUrls: ["nats://127.0.0.1:4222"],
    ompInvocation: "/opt/omp",
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
    gates: { design: "root-issues", merge: "human" },
    githubApps: {},
    stateDir,
  };
}

function manager(
  stateDir: string,
  run: ProcessManagerDeps["run"]
): {
  manager: ProcessManager;
  state: LegionState;
  commands: string[][];
} {
  const commands: string[][] = [];
  const state = newLegionState("omp", 1);
  const deps: ProcessManagerDeps = {
    state,
    saveState: async () => {},
    config: daemonConfig(stateDir),
    ompInvocation: "/opt/omp",
    panePath: "/usr/bin",
    credentialHelper: "!/opt/legion credential",
    run: async (command, options) => {
      commands.push(command);
      return await run(command, options);
    },
    natsPublish: () => {},
    natsRequest: async () => JSON.stringify({ type: "ack" }),
    mintControllerCapability: async () => "controller-secret",
    mintBootToken: async () => "boot-token",
    mintWorkerBootToken: async () => "worker-boot-token",
    connectWorkerRpc: async () => {
      throw new Error("connectWorkerRpc is not exercised by this fixture");
    },
    revokeSessionCapability: () => {},
    provisioningToken: async () => "installation-token",
    statPrompt: async () => {},
    workerCatchup: {
      repo: "sjawhar/legion",
      runner: async () => ({ stdout: "[]", stderr: "", exitCode: 0 }),
      tokenManager: {
        getToken: async () => ({
          token: "token",
          expiresAt: "2099-01-01T00:00:00.000Z",
          gitIdentity: { name: "legion[bot]", email: "legion@example.test" },
        }),
      },
    },
    now: () => now,
    dispatchClient: fakeDispatchClient(),
  };
  return { manager: new ProcessManager(deps), state, commands };
}

it("marks each daemon-created tmux window with its Legion owner", async () => {
  const stateDir = await mkdtemp(path.join(os.tmpdir(), "legion-thermo-ops-"));
  try {
    const { manager: processes, commands } = manager(stateDir, async (command) => {
      if (command[0] === "tmux" && command[1] === "has-session") {
        return { stdout: "", exitCode: 1 };
      }
      if (command[0] === "tmux" && command[1] === "new-window") {
        return { stdout: "@2 %1 12345\n", exitCode: 0 };
      }
      return { stdout: "", exitCode: 0 };
    });

    await processes.ensureController();

    expect(commands).toContainEqual([
      "tmux",
      "set-option",
      "-w",
      "-t",
      "@2",
      "@legion_owner",
      "legion-omp",
    ]);
  } finally {
    await rm(stateDir, { recursive: true, force: true });
  }
});

it("reconciles only stale windows owned by this daemon", async () => {
  const stateDir = await mkdtemp(path.join(os.tmpdir(), "legion-thermo-ops-"));
  try {
    const {
      manager: processes,
      state,
      commands,
    } = manager(stateDir, async (command) => {
      if (command[0] !== "tmux" || command[1] !== "list-windows") {
        return { stdout: "", exitCode: 0 };
      }
      const format = command[command.indexOf("-F") + 1] ?? "";
      if (!format.includes("#{@legion_owner}")) {
        return { stdout: "@42\n@100\n@101\n@102\n", exitCode: 0 };
      }
      return {
        stdout: [
          `@42\tlegion-omp\t${now / 1000}`,
          `@100\t\t${now / 1000 - 121}`,
          `@101\tlegion-omp\t${now / 1000 - 121}`,
          `@102\tlegion-omp\t${now / 1000 - 119}`,
        ].join("\n"),
        exitCode: 0,
      };
    });
    state.controllerLocator = { tmuxSession: "legion-omp", tmuxWindowId: "@42" };
    await processes.reconcileTmuxWindows();

    expect(commands.filter((command) => command[1] === "kill-window")).toEqual([
      ["tmux", "kill-window", "-t", "@101"],
    ]);
  } finally {
    await rm(stateDir, { recursive: true, force: true });
  }
});

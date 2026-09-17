// Shared scaffolding for the real-process E2E tests (`real-shutdown-e2e.test.ts`,
// `real-ready-retry-e2e.test.ts`): a `ProcessManager` over a real `TmuxRuntime` whose `run` and
// `connectWorkerRpc` default to the real thing, a scratch directory registry, and the
// subprocess/socket helpers both files need. Parameterised by project so each test file's
// manager targets its own private tmux server (`tmux -L legion-<project>`); the tmux-specific
// window helpers (session, window, pane liveness) stay with the shutdown test, which is the only
// one that opens panes.
import { existsSync } from "node:fs";
import { mkdtemp, rm } from "node:fs/promises";
import os from "node:os";
import path from "node:path";
import { type DaemonConfig, repoForIssue } from "../config";
import type { LegionState } from "../legion-state";
import { locatorsForIssue, type ProcessManagerDeps } from "../processes";
import { TmuxRuntime, type TmuxRuntimeDeps } from "../runtime-tmux";
import { connectWorkerRpc } from "../worker-rpc";
import { fakeDispatchClient } from "./ci-fixtures";

/** The daemon CLI every spawned `legion worker-shim` subprocess in these tests runs through. */
export const CLI_ENTRYPOINT = path.join(import.meta.dir, "..", "..", "cli", "index.ts");
/** The OMP stand-in fixtures live beside the CLI's own tests. */
export const CLI_FIXTURES_DIR = path.join(
  import.meta.dir,
  "..",
  "..",
  "cli",
  "__tests__",
  "fixtures"
);

const tempDirs: string[] = [];

/** A fresh temporary directory, removed by `removeScratchDirs` (call it from `afterAll`). */
export async function scratchDir(prefix: string): Promise<string> {
  const dir = await mkdtemp(path.join(os.tmpdir(), `${prefix}-`));
  tempDirs.push(dir);
  return dir;
}

export async function removeScratchDirs(): Promise<void> {
  await Promise.all(tempDirs.splice(0).map((dir) => rm(dir, { recursive: true, force: true })));
}

export interface RunResult {
  stdout: string;
  stderr: string;
  exitCode: number;
}

/** Runs a real command to completion, capturing both streams. */
export async function run(
  command: string[],
  options?: { cwd?: string; env?: NodeJS.ProcessEnv }
): Promise<RunResult> {
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

/** Polls until a `legion worker-shim --socket` subprocess has bound `target` (up to ~4 s). */
export async function waitForSocket(target: string): Promise<void> {
  for (let attempt = 0; attempt < 200; attempt += 1) {
    if (existsSync(target)) return;
    await Bun.sleep(20);
  }
  throw new Error(`worker shim socket never appeared at ${target}`);
}

export function realDaemonConfig(
  project: string,
  stateDir: string,
  port: number,
  overrides: Partial<DaemonConfig> = {}
): DaemonConfig {
  return {
    project,
    legionId: "sjawhar/1",
    port,
    runtime: { name: "tmux" },
    daemonUrl: `http://127.0.0.1:${port}`,
    bind: "127.0.0.1",
    envoyUrl: "http://127.0.0.1:9020",
    natsUrls: ["nats://127.0.0.1:4222"],
    ompInvocation: "bun",
    ompLaunchPrefix: [],
    projects: {
      LEGSMOKE: { repo: "sjawhar/legion" },
      LEGION: { repo: "sjawhar/legion" },
    },
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
    workerRpcTimeoutSeconds: 5,
    workerIdleRetireSeconds: 600,
    slowCommandTimeoutSeconds: 300,
    workerStreamPort: 13371,
    gates: { design: "off" },
    githubApps: {},
    stateDir,
    ...overrides,
  };
}

/** Builds `ProcessManager` deps over a real `TmuxRuntime` on the project's private server. `run`
 * and `connectWorkerRpc` default to the real thing; a test that needs a fake supplies its own.
 * `commands`, when given, records every command the runner is asked to run. `sleep` is threaded
 * through to both the manager and the runtime (the ready-retry test collapses the backoff with it). */
export function realProcessManagerDeps(
  cfg: DaemonConfig,
  state: LegionState,
  commands?: string[][],
  overrides: {
    run?: ProcessManagerDeps["run"];
    connectWorkerRpc?: TmuxRuntimeDeps["connectWorkerRpc"];
    sleep?: ProcessManagerDeps["sleep"];
  } = {}
): ProcessManagerDeps {
  const runner: ProcessManagerDeps["run"] =
    overrides.run ??
    (commands
      ? async (command, options) => {
          commands.push(command);
          return run(command, options);
        }
      : run);
  const runtime = new TmuxRuntime({
    tmux: { run: runner, socket: `legion-${state.project}` },
    project: state.project,
    stateDir: cfg.stateDir,
    ompInvocation: cfg.ompInvocation,
    ompLaunchPrefix: cfg.ompLaunchPrefix,
    deploymentInstructionsFile: path.join(cfg.stateDir, "deployment-instructions.md"),
    statPrompt: async () => {},
    provisioningToken: async () => "installation-token",
    run: runner,
    repoForIssue: (issue) => repoForIssue(cfg, issue),
    credentialHelper: "!true",
    slowCommandTimeoutMs: cfg.slowCommandTimeoutSeconds * 1000,
    connectWorkerRpc: overrides.connectWorkerRpc ?? connectWorkerRpc,
    workerRpcTimeoutMs: () => cfg.workerRpcTimeoutSeconds * 1000,
    now: () => Date.now(),
    sleep: overrides.sleep,
    issueLocators: (issue) => locatorsForIssue(state, issue),
  });
  return {
    state,
    saveState: async () => {},
    config: cfg,
    runtime,
    run: runner,
    processPath: process.env.PATH ?? "",
    rolePromptsDir: path.join(cfg.stateDir, "role-prompts"),
    credentialHelper: "!true",
    sleep: overrides.sleep,
    publishRole: () => {},
    natsRequest: async () => JSON.stringify({ type: "ack" }),
    mintControllerCapability: async () => "controller-secret",
    mintBootToken: async () => "boot-token",
    mintWorkerBootToken: async () => "worker-boot-token",
    workerCatchup: {
      runner: async () => ({ stdout: "[]", stderr: "", exitCode: 0 }),
      baseEnv: {},
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
      ownerForIssue: (issue) => repoForIssue(cfg, issue).split("/")[0] as string,
    },
    now: () => Date.now(),
    dispatchClient: fakeDispatchClient(),
    revokeSessionCapability: () => {},
  };
}

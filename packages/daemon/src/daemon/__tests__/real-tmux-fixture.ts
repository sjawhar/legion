// Shared scaffolding for real-process tmux tests: a `ProcessManager` over a real `TmuxRuntime`,
// scratch directories, subprocess/socket helpers, and isolated private tmux sessions. Every
// test server gets a minted socket/session name and is torn down by that name only.
import { randomUUID } from "node:crypto";
import { existsSync } from "node:fs";
import { mkdtemp, rm } from "node:fs/promises";
import os from "node:os";
import path from "node:path";
import { type DaemonConfig, type GitHubAppRole, repoForIssue } from "../config";
import type { LegionState } from "../legion-state";
import { locatorsForIssue, type ProcessManagerDeps } from "../processes";
import { TmuxRuntime, type TmuxRuntimeDeps } from "../runtime-tmux";
import { connectWorkerRpc } from "../worker-rpc";
import { fakeDispatchClient, waitFor } from "./ci-fixtures";

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

/** One real-tmux test's private namespace. `teardown` is idempotent because tmux reports an
 * already-exited server without creating one; it can only address this run's minted socket. */
export interface TmuxTestServer {
  readonly project: string;
  readonly session: string;
  readonly socket: string;
  argv(...rest: string[]): string[];
  teardown(): Promise<void>;
}

/** Reaps this test's private tmux server the moment this process dies, even when that death is a
 * SIGKILL past every `try`/`finally` and `afterAll` -- the one path where a killed `bun test`
 * never runs its own teardown, leaving the pane's `legion worker-shim` (and the OMP stand-in
 * behind it) to keep the private server alive forever. `setsid` is not asked to `--fork`: a
 * process `Bun.spawn` opens is never its own process-group leader, so `setsid` calls `setsid()`
 * in place rather than forking, and the pid this function returns is the actual watchdog loop,
 * now in a session and process group of its own -- a SIGKILL to this test process's own group (or
 * process tree) cannot reach it. The loop polls only this process's own liveness -- there is no
 * server yet to check at arm time (`createTmuxTestServer`'s own doc comment: "There is no server
 * until the test's first tmux command"), so a session-existence check here would exit the watchdog
 * immediately, before the test ever creates one -- and `kill-server` is idempotent (tmux reports
 * an already-exited or never-created server without creating one), so running it unconditionally
 * once this process dies is safe whether or not a server ever existed. `teardown` below kills the
 * watchdog outright once its own `kill-session` returns, so the watchdog never outlives both the
 * test and the server it guards. */
function armOrphanWatchdog(session: string) {
  const parentPid = process.pid;
  const script = [
    `while kill -0 ${parentPid} 2>/dev/null; do sleep 3; done`,
    `tmux -L ${session} kill-server 2>/dev/null`,
  ].join("\n");
  const watchdog = Bun.spawn(["setsid", "sh", "-c", script], {
    stdio: ["ignore", "ignore", "ignore"],
  });
  watchdog.unref();
  return watchdog;
}

/** Names one real-tmux test's private server/session from a UUID rather than a fixed label.
 * There is no server until the test's first tmux command; teardown kills only the named session
 * -- never `kill-server` (the whole-server primitive `tmux-e2e-isolation.test.ts` refuses
 * anywhere in this source tree, #1208: a shared host could still be running another run's server
 * under a name this one's UUID happens to collide with, however unlikely, and `kill-server` would
 * tear down every session on it, not just this test's own). Since this session is always the
 * server's only one, killing it ends the server too -- tmux exits once its last session is gone
 * -- so this stays as complete a teardown as `kill-server` would be, just scoped to the one name
 * this run actually minted. A watchdog reaps that same private server if this process dies before
 * teardown runs (see `armOrphanWatchdog`): its own `kill-server` is the one exception the guard
 * does not need to cover, since it is a shell string the watchdog's script builds, addressed at
 * the exact socket this call already owns. The orphaned session is still standing when the
 * watchdog fires -- `kill-session -t <session>` would reach it exactly as `teardown` does -- so
 * `kill-server` here is simply equivalent, not the only option, and carries the same negligible
 * collision exposure either way. */
export function createTmuxTestServer(label: string): TmuxTestServer {
  const project = `${label}${randomUUID().replaceAll("-", "")}`;
  const session = `legion-${project}`;
  const argv = (...rest: string[]) => ["tmux", "-L", session, ...rest];
  const watchdog = armOrphanWatchdog(session);
  return {
    project,
    session,
    socket: session,
    argv,
    teardown: async () => {
      await run(argv("kill-session", "-t", session));
      watchdog.kill();
    },
  };
}

/** Waits until a `legion worker-shim --socket` subprocess has bound `target`, for up to 10 s: a
 * fresh `bun` child boots before it binds, and every caller's test allows 15 s or more. */
export async function waitForSocket(target: string): Promise<void> {
  await waitFor(() => existsSync(target), 10_000, `the worker shim socket at ${target}`);
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
    controllerRuntime: runtime,
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
        getToken: async (role: GitHubAppRole) => ({
          token: "worker-token",
          expiresAt: "2099-01-01T00:00:00.000Z",
          gitIdentity: {
            name: role === "review" ? "legion-review[bot]" : "legion-implement[bot]",
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

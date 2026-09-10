// Real-tmux, real-process E2E for the graceful-shutdown machinery: no fake tmux runner and no
// fake WorkerRpcClient. Spawns a real `legion worker-shim` subprocess (via the real CLI
// entrypoint) inside a real tmux session, wired to the two dedicated fixture "OMP" stand-ins
// (`stuck-omp-rpc.ts`, `self-report-omp-rpc.ts`) that a mocked `run`/`connectWorkerRpc` could
// never actually exercise: a shim whose wrapped process never reacts to stdin closing (proving
// `stopProcess`'s timeout-then-kill fallback actually kills a real pane), and a process that
// itself POSTs `/legion/v1/process/exit` from inside its own graceful-shutdown hook while
// `closeTree` is the one asking it to exit (proving that self-report path never deadlocks
// against a real HTTP round trip).
import { afterAll, describe, expect, it } from "bun:test";
import { existsSync } from "node:fs";
import { mkdtemp, rm, writeFile } from "node:fs/promises";
import os from "node:os";
import path from "node:path";
import { roleToken } from "@legion/contracts";
import { type LegionApi, type LegionApiDeps, startLegionApi } from "../api";
import type { DaemonConfig } from "../config";
import { newLegionState } from "../legion-state";
import { ProcessManager, type ProcessManagerDeps } from "../processes";
import { fakeDispatchClient } from "./ci-fixtures";

const SESSION = "legion-smoke-T8Shutdown";
const STUCK_OMP = path.join(
  import.meta.dir,
  "..",
  "..",
  "cli",
  "__tests__",
  "fixtures",
  "stuck-omp-rpc.ts"
);
const SELF_REPORT_OMP = path.join(
  import.meta.dir,
  "..",
  "..",
  "cli",
  "__tests__",
  "fixtures",
  "self-report-omp-rpc.ts"
);
const CLI_ENTRYPOINT = path.join(import.meta.dir, "..", "..", "cli", "index.ts");

const tempDirs: string[] = [];

async function scratchDir(): Promise<string> {
  const dir = await mkdtemp(path.join(os.tmpdir(), "legion-real-shutdown-e2e-"));
  tempDirs.push(dir);
  return dir;
}

async function run(
  command: string[],
  options?: { cwd?: string; env?: NodeJS.ProcessEnv }
): Promise<{ stdout: string; stderr: string; exitCode: number }> {
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

async function ensureSession(): Promise<void> {
  const listed = await run(["tmux", "has-session", "-t", SESSION]);
  if (listed.exitCode !== 0) {
    const created = await run([
      "tmux",
      "new-session",
      "-d",
      "-s",
      SESSION,
      "-x",
      "200",
      "-y",
      "50",
    ]);
    if (created.exitCode !== 0) {
      throw new Error(`Unable to create tmux session ${SESSION}: ${created.stderr}`);
    }
  }
}

/** Opens a fresh window in the smoke session running `legion worker-shim` around `innerCommand`,
 * returning its window and (sole) pane id. */
async function openShimWindow(
  socketPath: string,
  innerCommand: string,
  env: Record<string, string>
): Promise<{ windowId: string; paneId: string }> {
  const envPairs = Object.entries(env).flatMap(([key, value]) => ["-e", `${key}=${value}`]);
  const shellCommand = `${process.execPath} ${CLI_ENTRYPOINT} worker-shim --socket ${socketPath} -- bun ${innerCommand}`;
  const opened = await run([
    "tmux",
    "new-window",
    "-t",
    SESSION,
    "-P",
    "-F",
    "#{window_id} #{pane_id}",
    ...envPairs,
    shellCommand,
  ]);
  if (opened.exitCode !== 0) {
    throw new Error(`Unable to open shim window: ${opened.stderr}`);
  }
  const [windowId, paneId] = opened.stdout.trim().split(" ");
  if (!windowId || !paneId) {
    throw new Error(`tmux new-window did not report ids: ${opened.stdout}`);
  }
  return { windowId, paneId };
}

async function waitForSocket(target: string): Promise<void> {
  for (let attempt = 0; attempt < 200; attempt += 1) {
    if (existsSync(target)) return;
    await Bun.sleep(20);
  }
  throw new Error(`worker shim socket never appeared at ${target}`);
}

async function paneAlive(paneId: string): Promise<boolean> {
  const listed = await run(["tmux", "list-panes", "-a", "-F", "#{pane_id}"]);
  return listed.stdout.split("\n").includes(paneId);
}

/** Polls until tmux itself no longer lists `paneId`, up to `timeoutMs`. `closeTree` resolving
 * (or its own promise settling) only means the shim/OMP process has exited and the daemon
 * considers the tree closed -- tmux's own reaping of the now-dead pane out of its internal pane
 * table is a separate, asynchronous OS-level step that can lag behind by a a few milliseconds,
 * especially under CI's heavier scheduling contention. A single unretried `paneAlive` check right
 * after `closeTree` resolves races that lag directly. */
async function waitForPaneGone(paneId: string, timeoutMs = 5_000): Promise<boolean> {
  const deadline = Date.now() + timeoutMs;
  for (;;) {
    if (!(await paneAlive(paneId))) return false;
    if (Date.now() >= deadline) return true;
    await Bun.sleep(20);
  }
}

function config(
  stateDir: string,
  port: number,
  overrides: Partial<DaemonConfig> = {}
): DaemonConfig {
  return {
    project: "realshutdown",
    legionId: "sjawhar/1",
    port,
    envoyUrl: "http://127.0.0.1:9020",
    natsUrls: ["nats://127.0.0.1:4222"],
    ompInvocation: "bun",
    dispatchProject: "LEGSMOKE",
    repo: "sjawhar/legion",
    repos: ["sjawhar/legion"],
    appLogins: [],
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
    gates: { design: "off", merge: "off" },
    githubApps: {},
    stateDir,
    ...overrides,
  };
}

function processManagerDeps(
  cfg: DaemonConfig,
  state: ReturnType<typeof newLegionState>,
  commands?: string[][]
): ProcessManagerDeps {
  return {
    state,
    saveState: async () => {},
    config: cfg,
    ompInvocation: cfg.ompInvocation,
    panePath: process.env.PATH ?? "",
    credentialHelper: "!true",
    run: commands
      ? async (command, options) => {
          commands.push(command);
          return run(command, options);
        }
      : run,
    natsPublish: () => {},
    natsRequest: async () => JSON.stringify({ type: "ack" }),
    mintControllerCapability: async () => "controller-secret",
    mintBootToken: async () => "boot-token",
    mintWorkerBootToken: async () => "worker-boot-token",
    connectWorkerRpc: (socketPath) =>
      import("../worker-rpc").then((m) => m.connectWorkerRpc(socketPath)),
    provisioningToken: async () => "installation-token",
    workerCatchup: {
      runner: async () => ({ stdout: "[]", stderr: "", exitCode: 0 }),
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
      repo: "sjawhar/legion",
    },
    now: () => Date.now(),
    dispatchClient: fakeDispatchClient(),
    revokeSessionCapability: () => {},
  };
}

afterAll(async () => {
  await run(["tmux", "kill-session", "-t", SESSION]);
  await Promise.all(tempDirs.splice(0).map((dir) => rm(dir, { recursive: true, force: true })));
});

describe("real graceful shutdown (tmux + worker-shim, no mocks)", () => {
  // Unlike its siblings below, this test needs no real tmux/shim process at all -- it proves a
  // lock-ordering property (a slow GitHub lease inside /worker/started must never hold the
  // per-token lock closeTree's own stop needs), so it runs unconditionally (not LEGION_E2E-gated)
  // against a real ProcessManager/HTTP daemon with a fake `run`/`connectWorkerRpc`.
  it("does not block closeTree's stop-then-delete for a worker whose /worker/started request is still waiting on its GitHub lease", async () => {
    const stateDir = await scratchDir();
    const root = "LEGION-9003";
    const state = newLegionState("realshutdown", 1);
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
    const token = roleToken("realshutdown", root, "tester");
    state.roles[token] = {
      issue: root,
      role: "tester",
      generation: 1,
      sessionId: "ses_tester",
      locator: {
        tmuxSession: "fake",
        tmuxWindowId: "@1",
        tmuxPaneId: "%1",
        socketPath: "/state/workers/tester.sock",
      },
    };

    const closed = Promise.withResolvers<void>();
    const fakeClient = {
      closed: closed.promise,
      runState: "idle" as const,
      negotiate: async () => {},
      prompt: async () => {},
      getState: async () => ({}),
      shutdown: () => {
        // Resolves gracefully on the next microtask, exactly like the shared fake client used
        // throughout processes.test.ts -- there is nothing hung about this worker's own stop;
        // the only thing gated in this test is the OTHER request's GitHub lease.
        queueMicrotask(() => closed.resolve());
      },
      close: () => closed.resolve(),
      onIdle: () => {},
    };

    const reachedLease = Promise.withResolvers<void>();
    const leaseGate = Promise.withResolvers<void>();
    const cfg = config(stateDir, 0, { treeStopTimeoutSeconds: 5 });
    const deps: ProcessManagerDeps = {
      ...processManagerDeps(cfg, state),
      run: async () => ({ stdout: "", stderr: "", exitCode: 0 }),
      connectWorkerRpc: async () => fakeClient,
    };
    const processes = new ProcessManager(deps);
    let daemon: LegionApi | undefined;
    try {
      const apiDeps: LegionApiDeps = {
        state,
        processManager: processes,
        dispatchClient: fakeDispatchClient(),
        tokenManager: {
          getToken: async () => {
            reachedLease.resolve();
            await leaseGate.promise;
            return {
              token: "minted-token",
              expiresAt: "2099-01-01T00:00:00.000Z",
              gitIdentity: { name: "legion-implement[bot]", email: "implement@example.com" },
            };
          },
        },
        envoyPublish: async () => {},
        onControllerReady: async () => {},
        onControllerEvent: async () => {},
      };
      daemon = startLegionApi(cfg, apiDeps);
      const port = daemon.server.port;

      const bootToken = await daemon.mintWorkerBootToken(root, root, "tester", 1);
      const requestPromise = fetch(`http://127.0.0.1:${port}/legion/v1/worker/started`, {
        method: "POST",
        headers: { "content-type": "application/json" },
        body: JSON.stringify({
          tree: root,
          issue: root,
          role: "tester",
          bootToken,
          sessionId: "ses_tester",
          agentId: "agt_tester",
          ompSessionFile: "/tmp/tester.json",
        }),
      });

      // Waits for the handler to have actually reached (and blocked inside) the GitHub lease
      // call before starting closeTree -- without this, closeTree could race ahead of the
      // request even reaching `mutateLiveRoleClaim` at all, proving nothing about which side of
      // the lock the lease sits on.
      await reachedLease.promise;

      let closeSettled = false;
      const closePromise = processes.closeTree(root).then(() => {
        closeSettled = true;
      });

      // Gives closeTree every real chance to finish its stop-then-delete for this exact token
      // while the /worker/started request above is still blocked on its GitHub lease -- proving
      // that lease is held OUTSIDE the per-token lock, not across it: if the lease instead ran
      // INSIDE mutateLiveRoleClaim's own callback, closeTree's attempt to acquire that same
      // token's lock for its stop would still be waiting, and this assertion would see
      // `closeSettled === false`.
      await new Promise((resolve) => setTimeout(resolve, 20));
      expect(closeSettled).toBe(true);
      expect(state.trees[root]?.status).toBe("closed");
      expect(state.roles[token]).toBeUndefined();

      leaseGate.resolve();
      const response = await requestPromise;
      await closePromise;

      // The request itself still resolves once its lease finally comes back -- as a 409, since
      // closeTree has by then deleted the claim it needed.
      expect(response.status).toBe(409);
    } finally {
      daemon?.stop();
    }
  });

  it.skipIf(process.env.LEGION_E2E !== "1")(
    "kills a real pane wrapping a real worker-shim after its stop timeout, when the wrapped process never reacts to stdin closing",
    async () => {
      await ensureSession();
      const stateDir = await scratchDir();
      const socketPath = path.join(stateDir, "stuck-tester.sock");
      const { windowId, paneId } = await openShimWindow(socketPath, STUCK_OMP, {});
      await waitForSocket(socketPath);
      expect(await paneAlive(paneId)).toBe(true);

      const root = "LEGION-9001";
      const state = newLegionState("realshutdown", 1);
      state.trees[root] = {
        root,
        generation: 1,
        status: "active",
        launchFailures: 0,
      };
      const token = roleToken("realshutdown", root, "tester");
      state.roles[token] = {
        issue: root,
        role: "tester",
        generation: 1,
        sessionId: "ses_tester",
        locator: {
          tmuxSession: SESSION,
          tmuxWindowId: windowId,
          tmuxPaneId: paneId,
          socketPath,
        },
      };
      const commands: string[][] = [];
      const workerStopTimeoutSeconds = 1;
      const processes = new ProcessManager(
        processManagerDeps(config(stateDir, 0, { workerStopTimeoutSeconds }), state, commands)
      );

      const startedAt = Date.now();
      await processes.closeTree(root);
      const elapsedMs = Date.now() - startedAt;

      expect(state.trees[root]?.status).toBe("closed");
      expect(state.roles[token]).toBeUndefined();
      expect(await waitForPaneGone(paneId)).toBe(false);
      // Proves this actually went through the timeout-then-kill fallback rather than closing
      // gracefully for an unrelated reason (e.g. a connect failure short-circuiting straight to
      // a kill): the wrapped process never reacts to stdin closing, so `stopProcess` must have
      // genuinely waited out the full configured grace window before it force-killed the pane.
      expect(elapsedMs).toBeGreaterThanOrEqual(workerStopTimeoutSeconds * 1000);
      expect(
        commands.some((command) => command[0] === "tmux" && command.includes("kill-pane"))
      ).toBe(true);
    },
    30_000
  );

  it.skipIf(process.env.LEGION_E2E !== "1")(
    "lets a root's own self-report POST to /process/exit resolve without deadlocking the closeTree that asked it to shut down",
    async () => {
      await ensureSession();
      const stateDir = await scratchDir();
      const socketPath = path.join(stateDir, "self-report-root.sock");
      const secretFile = path.join(stateDir, "secret.txt");
      const resultFile = path.join(stateDir, "result.txt");

      const root = "LEGION-9002";
      const state = newLegionState("realshutdown", 1);
      state.issues[root] = {
        key: root,
        title: "Root",
        status: "done",
        children: [],
      };
      state.trees[root] = {
        root,
        generation: 1,
        status: "active",
        launchFailures: 0,
      };

      let daemon: LegionApi | undefined;
      try {
        const cfg = config(stateDir, 0, { treeStopTimeoutSeconds: 5 });
        const deps = processManagerDeps(cfg, state);
        const processes = new ProcessManager(deps);
        const apiDeps: LegionApiDeps = {
          state,
          processManager: processes,
          dispatchClient: fakeDispatchClient(),
          tokenManager: {
            getToken: async () => {
              throw new Error("unused");
            },
          },
          envoyPublish: async () => {},
          onControllerReady: async () => {},
          onControllerEvent: async () => {},
        };
        daemon = startLegionApi(cfg, apiDeps);
        const port = daemon.server.port;

        const { windowId, paneId } = await openShimWindow(socketPath, SELF_REPORT_OMP, {
          LEGION_DAEMON_URL: `http://127.0.0.1:${port}`,
          LEGION_TREE: root,
          LEGION_GENERATION: "1",
          LEGION_SELF_REPORT_SESSION_ID: "ses_root",
          LEGION_SELF_REPORT_SECRET_FILE: secretFile,
          LEGION_SELF_REPORT_RESULT_FILE: resultFile,
        });
        await waitForSocket(socketPath);
        const rootTree = state.trees[root];
        if (!rootTree) throw new Error("test setup expects the root tree to already be recorded");
        rootTree.locator = {
          tmuxSession: SESSION,
          tmuxWindowId: windowId,
          tmuxPaneId: paneId,
          socketPath,
        };

        // Authenticates the root exactly through the real boot handshake (mint -> /process/started
        // -> a real, capability-backed secret) before the fixture ever POSTs /process/exit. Without
        // this, that POST 403s on an unauthenticated/fabricated secret, the fixture still exits
        // (mirroring the real hook, which exits regardless of the response status), and closeTree
        // still completes -- proving nothing about whether `reportRootExit` itself ever ran.
        const bootToken = await daemon.mintBootToken(root, 1);
        const started = await fetch(`http://127.0.0.1:${port}/legion/v1/process/started`, {
          method: "POST",
          headers: { "content-type": "application/json" },
          body: JSON.stringify({
            tree: root,
            generation: 1,
            rootSessionId: "ses_root",
            bootToken,
            agentId: "root-agent",
            ompSessionFile: "/tmp/self-report-root-session.json",
          }),
        });
        if (started.status !== 200) {
          throw new Error(`test setup failed to authenticate the root: ${started.status}`);
        }
        const { secret } = (await started.json()) as { secret: string };
        await writeFile(secretFile, secret, "utf8");

        // Never awaited before the self-report races in: `closeTree`'s root leg sends
        // `{type:"shutdown"}` and awaits the shim's socket closing, which only happens once the
        // fixture process (blocked reading stdin) sees EOF, POSTs `/process/exit`, and THAT
        // response returns -- exactly the sequence that deadlocks if the route ever joined this
        // same `closeTree` call.
        const closePromise = processes.closeTree(root);

        const timeout = Symbol("timeout");
        const result = await Promise.race([closePromise, Bun.sleep(15_000).then(() => timeout)]);

        expect(result).not.toBe(timeout);
        expect(state.trees[root]?.status).toBe("closed");
        expect(await waitForPaneGone(paneId)).toBe(false);
        // The one assertion that actually proves `reportRootExit` ran (not just that the pane
        // exited for some other reason): a 200 here is only reachable through the real,
        // capability-authenticated `/process/exit` -> `reportRootExit` path given this tree's
        // issue is recorded `closed` above -- a 403/409 would mean the fixture's own POST never
        // got past auth, and the fixture exits regardless of that response's status.
        expect(await Bun.file(resultFile).text()).toBe("200");
      } finally {
        daemon?.stop();
      }
    },
    30_000
  );
});

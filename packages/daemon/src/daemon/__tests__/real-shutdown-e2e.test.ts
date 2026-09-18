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
import { readFile, writeFile } from "node:fs/promises";
import path from "node:path";
import { roleToken } from "@legion/contracts";
import { type LegionApi, type LegionApiDeps, startLegionApi } from "../api";
import { newLegionState } from "../legion-state";
import { parseProcStatStartTicks } from "../proc-stat";
import { ProcessManager } from "../processes";
import type { TmuxLocator } from "../runtime";
import { fakeDispatchClient } from "./ci-fixtures";
import {
  CLI_ENTRYPOINT,
  CLI_FIXTURES_DIR,
  realDaemonConfig,
  realProcessManagerDeps,
  removeScratchDirs,
  run,
  scratchDir,
  waitForSocket,
} from "./real-tmux-fixture";

const PROJECT = "realshutdown";
const SESSION = "legion-smoke-T8Shutdown";
/** The `ProcessManager` under test has `project: "realshutdown"`, so every kill-pane it issues
 * targets exactly this private socket; the fixture's own tmux calls must land on the same server. */
const TMUX_SOCKET = `legion-${PROJECT}`;
const tmuxArgv = (...rest: string[]) => ["tmux", "-L", TMUX_SOCKET, ...rest];
const STUCK_OMP = path.join(CLI_FIXTURES_DIR, "stuck-omp-rpc.ts");
const SELF_REPORT_OMP = path.join(CLI_FIXTURES_DIR, "self-report-omp-rpc.ts");

async function ensureSession(): Promise<void> {
  const listed = await run(tmuxArgv("has-session", "-t", SESSION));
  if (listed.exitCode !== 0) {
    const created = await run(
      tmuxArgv("new-session", "-d", "-s", SESSION, "-x", "200", "-y", "50")
    );
    if (created.exitCode !== 0) {
      throw new Error(`Unable to create tmux session ${SESSION}: ${created.stderr}`);
    }
  }
}

/** Opens a fresh window in the smoke session running `legion worker-shim` around `innerCommand`,
 * returning the locator a real spawn would record for it: window and (sole) pane id plus the
 * pane's root pid and that process's real `/proc/<pid>/stat` start ticks. */
async function openShimWindow(
  socketPath: string,
  innerCommand: string,
  env: Record<string, string>
): Promise<TmuxLocator & { panePid: number; paneStartTicks: number; tmuxPaneId: string }> {
  const envPairs = Object.entries(env).flatMap(([key, value]) => ["-e", `${key}=${value}`]);
  const shellCommand = `${process.execPath} ${CLI_ENTRYPOINT} worker-shim --socket ${socketPath} -- bun ${innerCommand}`;
  const opened = await run(
    tmuxArgv(
      "new-window",
      "-t",
      SESSION,
      "-P",
      "-F",
      "#{window_id} #{pane_id} #{pane_pid}",
      ...envPairs,
      shellCommand
    )
  );
  if (opened.exitCode !== 0) {
    throw new Error(`Unable to open shim window: ${opened.stderr}`);
  }
  const [windowId, paneId, pidToken] = opened.stdout.trim().split(" ");
  const panePid = Number(pidToken);
  if (!windowId || !paneId || !Number.isSafeInteger(panePid) || panePid <= 0) {
    throw new Error(`tmux new-window did not report ids: ${opened.stdout}`);
  }
  const paneStartTicks = parseProcStatStartTicks(await readFile(`/proc/${panePid}/stat`, "utf8"));
  return {
    runtime: "tmux",
    tmuxSession: SESSION,
    tmuxWindowId: windowId,
    tmuxPaneId: paneId,
    socketPath,
    panePid,
    paneStartTicks,
  };
}

async function paneAlive(paneId: string): Promise<boolean> {
  const listed = await run(tmuxArgv("list-panes", "-a", "-F", "#{pane_id}"));
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

afterAll(async () => {
  await run(tmuxArgv("kill-server"));
  await removeScratchDirs();
});

describe("real graceful shutdown (tmux + worker-shim, no mocks)", () => {
  // Unlike its siblings below, this test needs no real tmux/shim process at all -- it proves a
  // lock-ordering property (a slow GitHub lease inside /worker/started must never hold the
  // per-token lock closeTree's own stop needs), so it runs unconditionally (not LEGION_E2E-gated)
  // against a real ProcessManager/HTTP daemon with a fake `run`/`connectWorkerRpc`.
  it("does not block closeTree's stop-then-delete for a worker whose /worker/started request is still waiting on its GitHub lease", async () => {
    const stateDir = await scratchDir("legion-real-shutdown-e2e");
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
        runtime: "tmux",
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
      adoptWorkingCopy: async () => {},
      prompt: async () => ({ turnStarted: Promise.resolve(), hasStarted: true, abandonWait() {} }),
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
    const cfg = realDaemonConfig(PROJECT, stateDir, 0, { treeStopTimeoutSeconds: 5 });
    const deps = realProcessManagerDeps(cfg, state, undefined, {
      run: async () => ({ stdout: "", stderr: "", exitCode: 0 }),
      connectWorkerRpc: async () => fakeClient,
    });
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
          pluginVersion: "1.49.0",
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
      const stateDir = await scratchDir("legion-real-shutdown-e2e");
      const socketPath = path.join(stateDir, "stuck-tester.sock");
      const opened = await openShimWindow(socketPath, STUCK_OMP, {});
      await waitForSocket(socketPath);
      expect(await paneAlive(opened.tmuxPaneId)).toBe(true);

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
        locator: opened,
      };
      const commands: string[][] = [];
      const workerStopTimeoutSeconds = 1;
      const processes = new ProcessManager(
        realProcessManagerDeps(
          realDaemonConfig(PROJECT, stateDir, 0, { workerStopTimeoutSeconds }),
          state,
          commands
        )
      );

      const startedAt = Date.now();
      await processes.closeTree(root);
      const elapsedMs = Date.now() - startedAt;

      expect(state.trees[root]?.status).toBe("closed");
      expect(state.roles[token]).toBeUndefined();
      expect(await waitForPaneGone(opened.tmuxPaneId)).toBe(false);
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
    "never kills a real pane whose recorded identity is not the process running in it: the stuck worker's claim clears, the tree closes, and the pane survives",
    async () => {
      await ensureSession();
      const stateDir = await scratchDir("legion-real-shutdown-e2e");
      const socketPath = path.join(stateDir, "reissued-tester.sock");
      const opened = await openShimWindow(socketPath, STUCK_OMP, {});
      await waitForSocket(socketPath);
      expect(await paneAlive(opened.tmuxPaneId)).toBe(true);

      const root = "LEGION-9004";
      const state = newLegionState("realshutdown", 1);
      state.trees[root] = {
        root,
        generation: 1,
        status: "active",
        launchFailures: 0,
      };
      const token = roleToken("realshutdown", root, "tester");
      // The same live pane, recorded as a process started one tick earlier: what a locator
      // looks like once tmux has reissued its pane id to some other role's process.
      state.roles[token] = {
        issue: root,
        role: "tester",
        generation: 1,
        sessionId: "ses_tester",
        locator: { ...opened, paneStartTicks: opened.paneStartTicks - 1 },
      };
      const commands: string[][] = [];
      const processes = new ProcessManager(
        realProcessManagerDeps(
          realDaemonConfig(PROJECT, stateDir, 0, { workerStopTimeoutSeconds: 1 }),
          state,
          commands
        )
      );

      try {
        await processes.closeTree(root);

        expect(state.trees[root]?.status).toBe("closed");
        expect(state.roles[token]).toBeUndefined();
        expect(
          commands.some((command) => command[0] === "tmux" && command.includes("kill-pane"))
        ).toBe(false);
        // The pane the daemon declined to kill is still running.
        expect(await paneAlive(opened.tmuxPaneId)).toBe(true);
      } finally {
        await run(tmuxArgv("kill-pane", "-t", opened.tmuxPaneId));
      }
    },
    30_000
  );

  it.skipIf(process.env.LEGION_E2E !== "1")(
    "lets a root's own self-report POST to /process/exit resolve without deadlocking the closeTree that asked it to shut down",
    async () => {
      await ensureSession();
      const stateDir = await scratchDir("legion-real-shutdown-e2e");
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
        const cfg = realDaemonConfig(PROJECT, stateDir, 0, { treeStopTimeoutSeconds: 5 });
        const deps = realProcessManagerDeps(cfg, state);
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

        const opened = await openShimWindow(socketPath, SELF_REPORT_OMP, {
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
        rootTree.locator = opened;

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
            pluginVersion: "1.49.0",
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
        expect(await waitForPaneGone(opened.tmuxPaneId)).toBe(false);
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

  it.skipIf(process.env.LEGION_E2E !== "1").each([
    ["done", "LEGION-9003"],
    ["backlog", "LEGION-9004"],
  ] as const)(
    "linger stops a real root pane through its shim on a %s issue: the root's own /process/exit answers 200, the pane is gone, and the tree stays lingering with its session file kept (LEGION-105)",
    async (issueStatus, root) => {
      await ensureSession();
      const stateDir = await scratchDir("legion-real-shutdown-e2e");
      const socketPath = path.join(stateDir, "linger-root.sock");
      const secretFile = path.join(stateDir, "secret.txt");
      const resultFile = path.join(stateDir, "result.txt");

      const state = newLegionState("realshutdown", 1);
      state.issues[root] = { key: root, title: "Root", status: issueStatus, children: [] };
      state.trees[root] = { root, generation: 1, status: "active", launchFailures: 0 };
      state.admission.active.push(root);

      let daemon: LegionApi | undefined;
      try {
        const cfg = realDaemonConfig(PROJECT, stateDir, 0, {
          treeStopTimeoutSeconds: 5,
          lingerHours: 1,
        });
        const deps = realProcessManagerDeps(cfg, state);
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

        const opened = await openShimWindow(socketPath, SELF_REPORT_OMP, {
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
        rootTree.locator = opened;

        // The real boot handshake (mint -> /process/started -> a capability-backed secret), so the
        // fixture's own POST /process/exit authenticates and its recorded status proves the route
        // ran `reportRootExit` (see the self-report test above).
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
            ompSessionFile: "/tmp/linger-root-session.json",
            pluginVersion: "1.49.0",
          }),
        });
        if (started.status !== 200) {
          throw new Error(`test setup failed to authenticate the root: ${started.status}`);
        }
        const { secret } = (await started.json()) as { secret: string };
        await writeFile(secretFile, secret, "utf8");

        // The linger itself resolves at once (the stop is started, never awaited); joining the
        // retire it started is what waits for the real shutdown round trip.
        await processes.beginLinger(root);
        expect(state.trees[root]).toMatchObject({ status: "lingering" });
        const timeout = Symbol("timeout");
        const result = await Promise.race([
          processes.retireTreeProcesses(root),
          Bun.sleep(15_000).then(() => timeout),
        ]);

        expect(result).not.toBe(timeout);
        expect(await waitForPaneGone(opened.tmuxPaneId)).toBe(false);
        // 200 is reachable only through the capability-authenticated `/process/exit` ->
        // `reportRootExit` path while the retire held `closingTrees`; on a `backlog` issue that
        // route branch is the tree being lingering, never the issue being done.
        expect(await Bun.file(resultFile).text()).toBe("200");
        expect(state.trees[root]).toMatchObject({
          status: "lingering",
          resumeSessionFile: "/tmp/linger-root-session.json",
        });
        expect(state.trees[root]?.locator).toBeUndefined();
        expect(state.roles[roleToken(PROJECT, root, "architect")]).toBeUndefined();
      } finally {
        daemon?.stop();
      }
    },
    30_000
  );
});

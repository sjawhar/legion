// Real-process E2E for the ready-time delivery retry (LEGION-39): a real `legion worker-shim`
// subprocess around `delayed-start-omp-rpc.ts`, an OMP stand-in whose first
// `negotiate_protocol` answer is late. It drives the real `connectWorkerRpc` over a unix socket
// with only tmux's `run` faked and the injected retry sleep collapsed. It proves the first
// connection's timed-out request closes, the shim accepts a second connection while the late
// answer is pending, and the second negotiate and prompt reach the real worker exactly once.
// This starts a real subprocess, so it is LEGION_E2E-gated.
import { afterAll, describe, expect, it, vi } from "bun:test";
import { existsSync } from "node:fs";
import { readFile } from "node:fs/promises";
import path from "node:path";
import { roleToken } from "@legion/contracts";
import { newLegionState, type WorkerRoleClaim } from "../legion-state";
import { ProcessManager } from "../processes";
import { connectWorkerRpc } from "../worker-rpc";
import {
  CLI_ENTRYPOINT,
  CLI_FIXTURES_DIR,
  realDaemonConfig,
  realProcessManagerDeps,
  removeScratchDirs,
  scratchDir,
  waitForSocket,
} from "./real-tmux-fixture";

const PROJECT = "realreadyretry";
const ROOT = "LEGION-9039";
const DELAYED_START_OMP = path.join(CLI_FIXTURES_DIR, "delayed-start-omp-rpc.ts");

afterAll(async () => {
  await removeScratchDirs();
});

/** Spawns the real shim around the delayed-start stand-in and seeds a booting tester claim whose
 * locator names the shim's socket, exactly as `/worker/ready` finds it. */
async function shimFixture(env: Record<string, string>) {
  const stateDir = await scratchDir("legion-real-ready-retry-e2e");
  const socketPath = path.join(stateDir, "tester.sock");
  const promptLog = path.join(stateDir, "prompts.log");
  const shim = Bun.spawn(
    [
      process.execPath,
      CLI_ENTRYPOINT,
      "worker-shim",
      "--socket",
      socketPath,
      "--",
      "bun",
      DELAYED_START_OMP,
    ],
    {
      stdout: "ignore",
      stderr: "inherit",
      env: { ...process.env, FIXTURE_PROMPT_LOG: promptLog, ...env },
    }
  );
  await waitForSocket(socketPath);

  const state = newLegionState(PROJECT, 1);
  state.issues[ROOT] = { key: ROOT, title: "Root", status: "in_progress", children: [] };
  state.trees[ROOT] = { root: ROOT, generation: 1, status: "active", launchFailures: 0 };
  const token = roleToken(PROJECT, ROOT, "tester");
  state.roles[token] = {
    issue: ROOT,
    role: "tester",
    generation: 1,
    sessionId: "ses_tester",
    pendingAssignment: {
      kind: "assignment",
      task: "verify #41",
      queuedAt: "2026-09-01T00:00:00.000Z",
      deliveryId: "00000000-0000-4000-8000-000000000039",
    },
    locator: {
      runtime: "tmux",
      tmuxSession: `legion-${PROJECT}`,
      tmuxWindowId: "@1",
      tmuxPaneId: "%1",
      socketPath,
    },
  };
  let connectCalls = 0;
  const cfg = realDaemonConfig(PROJECT, stateDir, 0, {
    workerRpcTimeoutSeconds: 1,
    // The injected `sleep` below resolves at once; a live idle-retire clock would re-arm itself
    // in a tight loop the moment the real worker reports `agent_end` (the role IS the active
    // phase, so the clock declines and re-arms), starving the test. Same reason
    // processes.test.ts's `config()` disables it by default.
    workerIdleRetireSeconds: 0,
  });
  const processes = new ProcessManager(
    realProcessManagerDeps(cfg, state, undefined, {
      // No pane exists: every tmux call the manager makes (a kill-pane at most) succeeds inertly.
      run: async () => ({ stdout: "", stderr: "", exitCode: 0 }),
      connectWorkerRpc: (target, timeoutMs) => {
        connectCalls += 1;
        return connectWorkerRpc(target, timeoutMs);
      },
      // Collapses every backoff sleep: the schedule is proven by processes.test.ts's fake clock;
      // this file proves the socket-level behavior between attempts.
      sleep: async () => {},
    })
  );
  processes.enableLaunches();
  const claim = (): WorkerRoleClaim => {
    const current = state.roles[token];
    if (!current || !("issue" in current)) throw new Error("tester claim disappeared");
    return current;
  };
  return {
    processes,
    state,
    token,
    claim,
    promptLog,
    connectCalls: () => connectCalls,
    async close() {
      processes.dispose();
      shim.kill();
      await shim.exited;
    },
  };
}

describe("real ready-time delivery retry (worker-shim socket, real connectWorkerRpc)", () => {
  it.skipIf(process.env.LEGION_E2E !== "1")(
    "delivers the queued assignment on the second attempt after the shim's first negotiate_protocol answers late",
    async () => {
      const errorLog = vi.spyOn(console, "error").mockImplementation(() => {});
      const fixture = await shimFixture({
        FIXTURE_FIRST_NEGOTIATE_RESPONSE_DELAY_MS: "2000",
        FIXTURE_AGENT_START_DELAY_MS: "0",
      });
      try {
        const startedAt = Date.now();
        await fixture.processes.workerReady(ROOT, "tester", "ses_tester", 1);
        const elapsedMs = Date.now() - startedAt;

        // Attempt 1's negotiate ran the full 1 s timeout before attempt 2 could connect.
        expect(elapsedMs).toBeGreaterThanOrEqual(1_000);
        expect(fixture.connectCalls()).toBe(2);
        expect(existsSync(fixture.promptLog)).toBe(true);
        expect((await readFile(fixture.promptLog, "utf8")).split("\n").filter(Boolean)).toEqual([
          "verify #41",
        ]);
        expect(fixture.claim().pendingAssignment).toBeUndefined();
        expect(fixture.claim().readyConfirmedAt).toBeDefined();
        expect(fixture.claim().launchFailures).toBeUndefined();
        expect(fixture.claim().promptFailures).toBe(0);
        expect(fixture.state.phases[ROOT]).toMatchObject({
          phase: "tester",
          sessionId: "ses_tester",
        });

        const calls = errorLog.mock.calls;
        const lines = calls.map((call) => String(call[0]));
        const retry = lines.find((line) =>
          line.includes("worker/ready delivery for LEGION-9039/tester failed")
        );
        expect(retry).toMatch(
          /worker\/ready delivery for LEGION-9039\/tester failed \(attempt 1\/6, cycle 1\); retrying in 5s: Worker RPC "negotiate_protocol" timed out after 1000ms/
        );
        // Exactly one retry line and no second cycle: the second attempt delivered. (LEGION-60's
        // turn-start observation may add its own `get_state` line; it is not a retry.)
        expect(lines.filter((line) => line.includes("retrying in"))).toHaveLength(1);
        expect(lines.some((line) => line.includes("exhausted cycle"))).toBeFalse();
        expect(lines.some((line) => line.includes("started no turn"))).toBeFalse();
      } finally {
        await fixture.close();
        errorLog.mockRestore();
      }
    },
    15_000
  );
});

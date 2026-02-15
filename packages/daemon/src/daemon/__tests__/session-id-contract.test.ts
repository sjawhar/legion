import { afterEach, describe, expect, it } from "bun:test";
import { mkdtemp, rm } from "node:fs/promises";
import os from "node:os";
import path from "node:path";
import { computeSessionId } from "../../state/types";
import type { SpawnOptions, WorkerEntry } from "../serve-manager";
import type { PortAllocatorInterface, ServeManagerInterface } from "../server";
import { startServer } from "../server";

class TestPortAllocator implements PortAllocatorInterface {
  private nextPort: number;

  constructor(startPort = 16000) {
    this.nextPort = startPort;
  }

  allocate(): number {
    const port = this.nextPort;
    this.nextPort += 1;
    return port;
  }

  release(_port: number): void {}
}

describe("sessionId contract (daemon vs state)", () => {
  let tempDir: string | null = null;
  let stopServer: (() => void) | null = null;

  const originalFetch = globalThis.fetch;
  const teamId = "123e4567-e89b-12d3-a456-426614174000";

  afterEach(async () => {
    globalThis.fetch = originalFetch;
    if (stopServer) {
      stopServer();
      stopServer = null;
    }
    if (tempDir) {
      await rm(tempDir, { recursive: true, force: true });
      tempDir = null;
    }
  });

  it("uses the same deterministic sessionId as the state machine (case-insensitive issue IDs)", async () => {
    const portAllocator = new TestPortAllocator(16500);
    const serveManager: ServeManagerInterface = {
      spawnServe: async (opts: SpawnOptions): Promise<WorkerEntry> => ({
        id: `${opts.issueId}-${opts.mode}`,
        port: opts.port,
        pid: 1234,
        sessionId: opts.sessionId,
        workspace: opts.workspace,
        startedAt: "2026-02-01T00:00:00.000Z",
        status: "starting",
        crashCount: 0,
        lastCrashAt: null,
      }),
      killWorker: async () => {},
      initializeSession: async () => {},
      healthCheck: async () => true,
    };

    tempDir = await mkdtemp(path.join(os.tmpdir(), "legion-session-contract-"));
    const stateFilePath = path.join(tempDir, "workers.json");
    const { server, stop } = startServer({
      port: 0,
      hostname: "127.0.0.1",
      teamId,
      legionDir: tempDir,
      shortId: "test",
      serveManager,
      portAllocator,
      stateFilePath,
    });
    stopServer = stop;

    const baseUrl = `http://127.0.0.1:${server.port}`;
    const response = await originalFetch(`${baseUrl}/workers`, {
      method: "POST",
      headers: { "content-type": "application/json" },
      body: JSON.stringify({
        issueId: "ENG-42",
        mode: "implement",
        workspace: "/tmp/work",
      }),
    });

    expect(response.status).toBe(200);
    const body = (await response.json()) as { sessionId: string };
    expect(body.sessionId).toBe(computeSessionId(teamId, "ENG-42", "implement"));
  });
});

import { afterEach, describe, expect, it, mock } from "bun:test";
import { mkdtemp, rm } from "node:fs/promises";
import os from "node:os";
import path from "node:path";
import { computeSessionId } from "../../state/types";
import type { SpawnOptions, WorkerEntry } from "../serve-manager";
import { type PortAllocatorInterface, type ServeManagerInterface, startServer } from "../server";
import { type PersistedWorkerState, writeStateFile } from "../state-file";

let mockSessionStatus: (() => Promise<unknown>) | null = null;

mock.module("@opencode-ai/sdk/v2", () => ({
  createOpencodeClient: () => ({
    session: {
      status: async () => {
        if (mockSessionStatus) {
          return mockSessionStatus();
        }
        throw new Error("No mock configured");
      },
    },
  }),
}));

class TestPortAllocator implements PortAllocatorInterface {
  private nextPort: number;
  public readonly released: number[] = [];

  constructor(startPort = 15000) {
    this.nextPort = startPort;
  }

  allocate(): number {
    const port = this.nextPort;
    this.nextPort += 1;
    return port;
  }

  release(port: number): void {
    this.released.push(port);
  }
}

describe("daemon server", () => {
  let tempDir: string | null = null;
  let stopServer: (() => void) | null = null;
  let baseUrl = "";
  let serveManager: ServeManagerInterface;
  let portAllocator: TestPortAllocator;
  let spawnCalls: SpawnOptions[] = [];
  let killCalls: WorkerEntry[] = [];
  const originalFetch = globalThis.fetch;
  const teamId = "123e4567-e89b-12d3-a456-426614174000";

  async function startTestServer(options?: {
    state?: PersistedWorkerState;
    serveManagerOverrides?: Partial<ServeManagerInterface>;
    portAllocatorOverride?: TestPortAllocator;
  }) {
    spawnCalls = [];
    killCalls = [];
    portAllocator = options?.portAllocatorOverride ?? new TestPortAllocator(15500);
    serveManager = {
      spawnServe: async (opts: SpawnOptions) => {
        spawnCalls.push(opts);
        return {
          id: `${opts.issueId}-${opts.mode}`,
          port: opts.port,
          pid: 1234,
          sessionId: opts.sessionId,
          startedAt: "2026-02-01T00:00:00.000Z",
          status: "starting",
          crashCount: 0,
          lastCrashAt: null,
        };
      },
      killWorker: async (entry: WorkerEntry) => {
        killCalls.push(entry);
      },
      healthCheck: async () => true,
    };
    if (options?.serveManagerOverrides) {
      serveManager = { ...serveManager, ...options.serveManagerOverrides };
    }

    tempDir = await mkdtemp(path.join(os.tmpdir(), "legion-server-"));
    const stateFilePath = path.join(tempDir, "workers.json");
    if (options?.state) {
      await writeStateFile(stateFilePath, options.state);
    }
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
    baseUrl = `http://127.0.0.1:${server.port}`;
  }

  async function requestJson(pathname: string, init?: RequestInit) {
    const response = await originalFetch(`${baseUrl}${pathname}`, {
      ...init,
      headers: {
        "content-type": "application/json",
        ...(init?.headers ?? {}),
      },
    });
    return response;
  }

  afterEach(async () => {
    globalThis.fetch = originalFetch;
    mockSessionStatus = null;
    if (stopServer) {
      stopServer();
      stopServer = null;
    }
    if (tempDir) {
      await rm(tempDir, { recursive: true, force: true });
      tempDir = null;
    }
  });

  it("returns health data", async () => {
    await startTestServer();
    const response = await requestJson("/health");
    expect(response.status).toBe(200);
    const body = (await response.json()) as {
      status: string;
      uptime: number;
      workerCount: number;
    };
    expect(body.status).toBe("ok");
    expect(typeof body.uptime).toBe("number");
    expect(body.workerCount).toBe(0);
  });

  it("lists workers", async () => {
    await startTestServer();
    const response = await requestJson("/workers");
    expect(response.status).toBe(200);
    const body = (await response.json()) as WorkerEntry[];
    expect(body).toEqual([]);
  });

  it("rejects invalid worker creation payloads", async () => {
    await startTestServer();
    const response = await requestJson("/workers", {
      method: "POST",
      body: JSON.stringify({}),
    });
    expect(response.status).toBe(400);
  });

  it("creates workers", async () => {
    await startTestServer();
    const response = await requestJson("/workers", {
      method: "POST",
      body: JSON.stringify({
        issueId: "ENG-42",
        mode: "implement",
        workspace: "/tmp/work",
        env: { DEBUG: "1" },
      }),
    });

    expect(response.status).toBe(200);
    const body = (await response.json()) as { id: string; port: number; sessionId: string };
    expect(body.id).toBe("eng-42-implement");
    expect(body.port).toBe(15500);
    expect(body.sessionId).toBe(computeSessionId(teamId, "eng-42", "implement"));

    expect(spawnCalls.length).toBe(1);
    expect(spawnCalls[0]).toMatchObject({
      issueId: "eng-42",
      mode: "implement",
      workspace: "/tmp/work",
      port: 15500,
      env: { DEBUG: "1" },
    });

    const listResponse = await requestJson("/workers");
    const listBody = (await listResponse.json()) as WorkerEntry[];
    expect(listBody.length).toBe(1);

    const entryResponse = await requestJson(`/workers/${body.id}`);
    expect(entryResponse.status).toBe(200);
    const entryBody = (await entryResponse.json()) as WorkerEntry;
    expect(entryBody.port).toBe(15500);
  });

  it("rejects duplicate worker for same issue+mode", async () => {
    await startTestServer();
    const res1 = await requestJson("/workers", {
      method: "POST",
      body: JSON.stringify({ issueId: "ENG-1", mode: "implement", workspace: "/tmp" }),
    });
    expect(res1.status).toBe(200);

    const res2 = await requestJson("/workers", {
      method: "POST",
      body: JSON.stringify({ issueId: "ENG-1", mode: "implement", workspace: "/tmp" }),
    });
    expect(res2.status).toBe(409);
    const body = (await res2.json()) as { error: string };
    expect(body.error).toBe("worker_already_exists");
  });

  it("waits for state load before checking duplicates", async () => {
    let healthGateResolve!: () => void;
    const healthGate = new Promise<void>((resolve) => {
      healthGateResolve = () => resolve();
    });
    const existing: WorkerEntry = {
      id: "eng-1-implement",
      port: 15510,
      pid: 4321,
      sessionId: computeSessionId(teamId, "eng-1", "implement"),
      startedAt: "2026-02-01T00:00:00.000Z",
      status: "running",
      crashCount: 0,
      lastCrashAt: null,
    };

    await startTestServer({
      state: {
        workers: {
          [existing.id]: existing,
        },
        crashHistory: {},
      },
      serveManagerOverrides: {
        healthCheck: async () => {
          await healthGate;
          return true;
        },
      },
    });

    const responsePromise = requestJson("/workers", {
      method: "POST",
      body: JSON.stringify({ issueId: "ENG-1", mode: "implement", workspace: "/tmp" }),
    });

    const early = await Promise.race([
      responsePromise.then(() => "resolved" as const),
      new Promise<"pending">((resolve) => {
        setTimeout(() => resolve("pending"), 20);
      }),
    ]);

    expect(early).toBe("pending");
    healthGateResolve();
    const response = await responsePromise;
    expect(response.status).toBe(409);
  });

  it("allows respawn for dead workers", async () => {
    await startTestServer();
    const createResponse = await requestJson("/workers", {
      method: "POST",
      body: JSON.stringify({ issueId: "ENG-5", mode: "implement", workspace: "/tmp" }),
    });
    const created = (await createResponse.json()) as { id: string };

    await requestJson(`/workers/${created.id}`, {
      method: "PATCH",
      body: JSON.stringify({
        status: "dead",
        crashCount: 1,
        lastCrashAt: new Date().toISOString(),
      }),
    });

    const respawn = await requestJson("/workers", {
      method: "POST",
      body: JSON.stringify({ issueId: "ENG-5", mode: "implement", workspace: "/tmp" }),
    });
    expect(respawn.status).toBe(200);
    expect(portAllocator.released).toEqual([15500]);
  });

  it("resets crash history via endpoint", async () => {
    await startTestServer();
    const createResponse = await requestJson("/workers", {
      method: "POST",
      body: JSON.stringify({ issueId: "ENG-6", mode: "implement", workspace: "/tmp" }),
    });
    const created = (await createResponse.json()) as { id: string };

    await requestJson(`/workers/${created.id}`, {
      method: "PATCH",
      body: JSON.stringify({
        status: "dead",
        crashCount: 3,
        lastCrashAt: new Date().toISOString(),
      }),
    });

    const resetResponse = await requestJson(`/workers/${created.id}/crashes`, {
      method: "DELETE",
    });
    expect(resetResponse.status).toBe(200);
    expect(await resetResponse.json()).toEqual({ reset: true, id: created.id });

    const respawn = await requestJson("/workers", {
      method: "POST",
      body: JSON.stringify({ issueId: "ENG-6", mode: "implement", workspace: "/tmp" }),
    });
    expect(respawn.status).toBe(200);
  });

  it("auto-resets crash history after cooldown", async () => {
    const workerId = "eng-7-implement";
    const oldCrashAt = new Date(Date.now() - 2 * 60 * 60 * 1000).toISOString();
    await startTestServer({
      state: {
        workers: {},
        crashHistory: {
          [workerId]: { crashCount: 3, lastCrashAt: oldCrashAt },
        },
      },
    });

    const response = await requestJson("/workers", {
      method: "POST",
      body: JSON.stringify({ issueId: "ENG-7", mode: "implement", workspace: "/tmp" }),
    });
    expect(response.status).toBe(200);
  });

  it("returns 404 for missing worker", async () => {
    await startTestServer();
    const response = await requestJson("/workers/unknown");
    expect(response.status).toBe(404);
  });

  it("deletes workers", async () => {
    await startTestServer();
    const createResponse = await requestJson("/workers", {
      method: "POST",
      body: JSON.stringify({
        issueId: "ENG-99",
        mode: "implement",
        workspace: "/tmp/work",
      }),
    });
    const created = (await createResponse.json()) as { id: string; port: number };

    const deleteResponse = await requestJson(`/workers/${created.id}`, { method: "DELETE" });
    expect(deleteResponse.status).toBe(200);
    expect(await deleteResponse.json()).toEqual({ status: "stopped" });
    expect(killCalls.length).toBe(1);
    expect(portAllocator.released).toEqual([15500]);

    const listResponse = await requestJson("/workers");
    const listBody = (await listResponse.json()) as WorkerEntry[];
    expect(listBody).toEqual([]);
  });

  it("returns status from worker", async () => {
    await startTestServer();
    const createResponse = await requestJson("/workers", {
      method: "POST",
      body: JSON.stringify({
        issueId: "ENG-10",
        mode: "implement",
        workspace: "/tmp/work",
      }),
    });
    const created = (await createResponse.json()) as {
      id: string;
      port: number;
      sessionId: string;
    };

    mockSessionStatus = async () => ({
      data: { status: "active", sessionId: created.sessionId },
    });

    const response = await requestJson(`/workers/${created.id}/status`);
    expect(response.status).toBe(200);
    expect(await response.json()).toEqual({ status: "active", sessionId: created.sessionId });
  });

  it("returns 502 when worker unreachable", async () => {
    await startTestServer();
    const createResponse = await requestJson("/workers", {
      method: "POST",
      body: JSON.stringify({
        issueId: "ENG-11",
        mode: "implement",
        workspace: "/tmp/work",
      }),
    });
    const created = (await createResponse.json()) as {
      id: string;
      port: number;
      sessionId: string;
    };

    mockSessionStatus = async () => {
      throw new Error("boom");
    };

    const response = await requestJson(`/workers/${created.id}/status`);
    expect(response.status).toBe(502);
  });

  it("returns 502 when worker returns error status", async () => {
    await startTestServer();
    const createResponse = await requestJson("/workers", {
      method: "POST",
      body: JSON.stringify({
        issueId: "ENG-12",
        mode: "implement",
        workspace: "/tmp/work",
      }),
    });
    const created = (await createResponse.json()) as {
      id: string;
      port: number;
      sessionId: string;
    };

    mockSessionStatus = async () => ({
      data: undefined,
      error: { message: "internal server error" },
    });

    const response = await requestJson(`/workers/${created.id}/status`);
    expect(response.status).toBe(502);
  });

  it("shuts down on request", async () => {
    let shutdownCalls = 0;
    await startTestServer();
    stopServer?.();
    stopServer = null;

    const { server, stop } = startServer({
      port: 0,
      hostname: "127.0.0.1",
      teamId,
      legionDir: tempDir ?? os.tmpdir(),
      shortId: "test",
      serveManager,
      portAllocator,
      stateFilePath: path.join(tempDir ?? os.tmpdir(), "workers.json"),
      shutdownFn: async () => {
        shutdownCalls += 1;
      },
    });
    stopServer = stop;
    baseUrl = `http://127.0.0.1:${server.port}`;

    const response = await requestJson("/shutdown", { method: "POST" });
    expect(response.status).toBe(200);
    expect(await response.json()).toEqual({ status: "shutting_down" });
    expect(shutdownCalls).toBe(1);
  });

  it("returns 404 for unknown routes", async () => {
    await startTestServer();
    const response = await requestJson("/nope");
    expect(response.status).toBe(404);
  });
});

import { afterEach, describe, expect, it, mock } from "bun:test";
import { mkdtemp, rm } from "node:fs/promises";
import os from "node:os";
import path from "node:path";
import { computeSessionId } from "../../state/types";
import type { SpawnOptions, WorkerEntry } from "../serve-manager";
import { type PortAllocatorInterface, type ServeManagerInterface, startServer } from "../server";

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

  async function startTestServer() {
    spawnCalls = [];
    killCalls = [];
    portAllocator = new TestPortAllocator(15500);
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
        };
      },
      killWorker: async (entry: WorkerEntry) => {
        killCalls.push(entry);
      },
      healthCheck: async () => true,
    };

    tempDir = await mkdtemp(path.join(os.tmpdir(), "legion-server-"));
    const stateFilePath = path.join(tempDir, "workers.json");
    const { server, stop } = startServer({
      port: 0,
      hostname: "127.0.0.1",
      teamId,
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

import { afterEach, describe, expect, it } from "bun:test";
import { mkdtemp, rm } from "node:fs/promises";
import os from "node:os";
import path from "node:path";
import type { SpawnOptions, WorkerEntry } from "../serve-manager";
import { startServer, type PortAllocatorInterface, type ServeManagerInterface } from "../server";

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
    expect(body.id).toBe("ENG-42-implement");
    expect(body.port).toBe(15500);
    expect(typeof body.sessionId).toBe("string");

    expect(spawnCalls.length).toBe(1);
    expect(spawnCalls[0]).toMatchObject({
      issueId: "ENG-42",
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
    const created = (await createResponse.json()) as { id: string; port: number; sessionId: string };

    globalThis.fetch = (async (input: Request | string) => {
      const url = typeof input === "string" ? input : input.url;
      if (url.includes(`/session/status`)) {
        return {
          ok: true,
          json: async () => ({ status: "active", sessionId: created.sessionId }),
        } as Response;
      }
      return originalFetch(input);
    }) as typeof fetch;

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
    const created = (await createResponse.json()) as { id: string; port: number; sessionId: string };

    globalThis.fetch = (async (input: Request | string) => {
      const url = typeof input === "string" ? input : input.url;
      if (url.includes(`/session/status`)) {
        throw new Error("boom");
      }
      return originalFetch(input);
    }) as typeof fetch;

    const response = await requestJson(`/workers/${created.id}/status`);
    expect(response.status).toBe(502);
  });

  it("returns 404 for unknown routes", async () => {
    await startTestServer();
    const response = await requestJson("/nope");
    expect(response.status).toBe(404);
  });
});

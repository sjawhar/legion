import { afterEach, describe, expect, it, mock } from "bun:test";
import { mkdtemp, rm } from "node:fs/promises";
import os from "node:os";
import path from "node:path";
import { computeSessionId } from "../../state/types";
import type { WorkerEntry } from "../serve-manager";
import { type ServeManagerInterface, startServer } from "../server";
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

const sharedServePort = 15500;

describe("daemon server", () => {
  let tempDir: string | null = null;
  let stopServer: (() => void) | null = null;
  let baseUrl = "";
  let serveManager: ServeManagerInterface;
  let createSessionCalls: Array<{ port: number; sessionId: string; workspace: string }> = [];
  const originalFetch = globalThis.fetch;
  const teamId = "123e4567-e89b-12d3-a456-426614174000";

  async function startTestServer(options?: {
    state?: PersistedWorkerState;
    serveManagerOverrides?: Partial<ServeManagerInterface>;
  }) {
    createSessionCalls = [];
    serveManager = {
      createSession: async (port, sessionId, workspace) => {
        createSessionCalls.push({ port, sessionId, workspace });
        return sessionId;
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
      serveManager,
      sharedServePort,
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

  it("rejects relative workspace paths", async () => {
    await startTestServer();
    const response = await requestJson("/workers", {
      method: "POST",
      body: JSON.stringify({
        issueId: "ENG-50",
        mode: "implement",
        workspace: "relative/path",
      }),
    });
    expect(response.status).toBe(400);
    const body = (await response.json()) as { error: string };
    expect(body.error).toBe("workspace must be an absolute path");
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
    expect(body.port).toBe(sharedServePort);
    expect(body.sessionId).toBe(computeSessionId(teamId, "eng-42", "implement"));

    expect(createSessionCalls.length).toBe(1);
    expect(createSessionCalls[0].port).toBe(sharedServePort);
    expect(createSessionCalls[0].workspace).toBe(tempDir);

    const listResponse = await requestJson("/workers");
    const listBody = (await listResponse.json()) as WorkerEntry[];
    expect(listBody.length).toBe(1);

    const entryResponse = await requestJson(`/workers/${body.id}`);
    expect(entryResponse.status).toBe(200);
    const entryBody = (await entryResponse.json()) as WorkerEntry;
    expect(entryBody.port).toBe(sharedServePort);
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

  it("loads persisted workers from state file", async () => {
    const existing: WorkerEntry = {
      id: "eng-1-implement",
      port: sharedServePort,
      sessionId: computeSessionId(teamId, "eng-1", "implement"),
      workspace: "/tmp",
      startedAt: "2026-02-01T00:00:00.000Z",
      status: "running",
      crashCount: 0,
      lastCrashAt: null,
    };

    await startTestServer({
      state: {
        workers: { [existing.id]: existing },
        crashHistory: {},
      },
    });

    const response = await requestJson("/workers", {
      method: "POST",
      body: JSON.stringify({ issueId: "ENG-1", mode: "implement", workspace: "/tmp" }),
    });
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

  describe("POST /state/collect", () => {
    it("returns collected state for linear backend", async () => {
      await startTestServer();
      const issues = [
        {
          identifier: "ENG-21",
          state: { name: "Todo" },
          labels: { nodes: [] },
        },
      ];
      const response = await requestJson("/state/collect", {
        method: "POST",
        body: JSON.stringify({ backend: "linear", issues }),
      });
      expect(response.status).toBe(200);
      const body = (await response.json()) as { issues: Record<string, unknown> };
      expect(body.issues).toBeDefined();
      expect(body.issues["ENG-21"]).toBeDefined();
    });

    it("returns collected state for github backend", async () => {
      await startTestServer();
      const issues = [
        {
          id: "PVTI_abc",
          content: {
            number: 42,
            repository: "acme/widgets",
            url: "https://github.com/acme/widgets/issues/42",
            type: "Issue",
          },
          status: "Todo",
          labels: [],
        },
      ];
      const response = await requestJson("/state/collect", {
        method: "POST",
        body: JSON.stringify({ backend: "github", issues }),
      });
      expect(response.status).toBe(200);
      const body = (await response.json()) as { issues: Record<string, unknown> };
      expect(body.issues).toBeDefined();
      expect(body.issues["acme-widgets-42"]).toBeDefined();
    });

    it("rejects invalid backend", async () => {
      await startTestServer();
      const response = await requestJson("/state/collect", {
        method: "POST",
        body: JSON.stringify({ backend: "jira", issues: [] }),
      });
      expect(response.status).toBe(400);
      const body = (await response.json()) as { error: string };
      expect(body.error).toBe("invalid_backend");
    });

    it("rejects missing issues field", async () => {
      await startTestServer();
      const response = await requestJson("/state/collect", {
        method: "POST",
        body: JSON.stringify({ backend: "linear" }),
      });
      expect(response.status).toBe(400);
    });

    it("rejects non-object issues field", async () => {
      await startTestServer();
      for (const issues of ["string", 42, true]) {
        const response = await requestJson("/state/collect", {
          method: "POST",
          body: JSON.stringify({ backend: "linear", issues }),
        });
        expect(response.status).toBe(400);
      }
    });

    it("returns empty state for empty issues array", async () => {
      await startTestServer();
      const response = await requestJson("/state/collect", {
        method: "POST",
        body: JSON.stringify({ backend: "linear", issues: [] }),
      });
      expect(response.status).toBe(200);
      const body = (await response.json()) as { issues: Record<string, unknown> };
      expect(body.issues).toEqual({});
    });
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
      serveManager,
      sharedServePort,
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

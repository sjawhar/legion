import { afterEach, beforeEach, describe, expect, it } from "bun:test";
import { mkdir, mkdtemp, rm } from "node:fs/promises";
import os from "node:os";
import path from "node:path";
import type { LegionPaths } from "../paths";
import { promoteSession } from "../promoted-sessions";
import type { RuntimeAdapter } from "../runtime/types";
import { startServer } from "../server";
import { type PersistedWorkerState, writeStateFile } from "../state-file";
import { createMockEnvoyServer, type MockEnvoyServer } from "./mock-envoy-server";

const sharedServePort = 15600;
const legionId = "test-promoted-routes";

describe("promoted routes", () => {
  let tempDir: string | null = null;
  let stopServer: (() => void) | null = null;
  let baseUrl = "";
  let mockEnvoy: MockEnvoyServer;

  beforeEach(() => {
    mockEnvoy = createMockEnvoyServer();
  });

  function makeAdapter(): RuntimeAdapter {
    return {
      start: async () => {},
      stop: async () => {},
      healthy: async () => true,
      getPort: () => sharedServePort,
      getServePid: () => 0,
      createSession: async (sessionId: string) => sessionId,
      sendPrompt: async () => {},
      getSessionStatus: async () => ({ data: undefined }),
      deleteSession: async () => {},
      sessionExists: async () => false,
    };
  }

  function makePaths(dir: string): LegionPaths {
    return {
      dataDir: path.join(dir, "data"),
      stateDir: path.join(dir, "state"),
      reposDir: path.join(dir, "data", "repos"),
      workspacesDir: path.join(dir, "data", "workspaces"),
      legionsFile: path.join(dir, "state", "legions.json"),
      forLegion: (projectId: string) => ({
        legionStateDir: path.join(dir, "state", "legions", projectId),
        workersFile: path.join(dir, "state", "legions", projectId, "workers.json"),
        promotedFile: path.join(dir, "state", "legions", projectId, "promoted.json"),
        feedbackFile: path.join(dir, "state", "legions", projectId, "feedback.jsonl"),
        logDir: path.join(dir, "state", "legions", projectId, "logs"),
        workspacesDir: path.join(dir, "data", "workspaces", projectId),
      }),
      repoClonePath: (host: string, owner: string, repo: string) =>
        path.join(dir, "data", "repos", host, owner, repo),
    };
  }

  async function startTestServer(options?: {
    state?: PersistedWorkerState;
    adapterOverrides?: Partial<RuntimeAdapter>;
  }) {
    tempDir = await mkdtemp(path.join(os.tmpdir(), "legion-promoted-routes-"));
    const paths = makePaths(tempDir);
    const stateFilePath = path.join(tempDir, "workers.json");
    if (options?.state) {
      await writeStateFile(stateFilePath, options.state);
    }
    // Ensure the legion state dir exists for promoted.json
    await mkdir(paths.forLegion(legionId).legionStateDir, { recursive: true });

    let adapter = makeAdapter();
    if (options?.adapterOverrides) {
      adapter = { ...adapter, ...options.adapterOverrides };
    }

    const { server, stop } = startServer({
      port: 0,
      hostname: "127.0.0.1",
      envoyUrl: mockEnvoy.url,
      legionId,
      paths,
      adapter,
      stateFilePath,
    });
    stopServer = stop;
    baseUrl = `http://127.0.0.1:${server.port}`;
  }

  async function requestJson(pathname: string, init?: RequestInit) {
    return fetch(`${baseUrl}${pathname}`, {
      ...init,
      headers: {
        "content-type": "application/json",
        ...(init?.headers ?? {}),
      },
    });
  }

  afterEach(async () => {
    if (stopServer) {
      stopServer();
      stopServer = null;
    }
    if (tempDir) {
      await rm(tempDir, { recursive: true, force: true });
      tempDir = null;
    }
    mockEnvoy.stop();
  });

  describe("GET /promoted", () => {
    it("returns empty array when no sessions promoted", async () => {
      await startTestServer();
      const response = await requestJson("/promoted");
      expect(response.status).toBe(200);
      const body = (await response.json()) as unknown[];
      expect(body).toEqual([]);
    });

    it("returns promoted sessions", async () => {
      await startTestServer();
      const paths = makePaths(tempDir as string);
      const promotedFile = paths.forLegion(legionId).promotedFile;
      await promoteSession(
        promotedFile,
        "ses_aabbccddee01ABCDEFghijklmn",
        "legion-po",
        "sjawhar/legion"
      );

      const response = await requestJson("/promoted");
      expect(response.status).toBe(200);
      const body = (await response.json()) as Array<{
        sessionId: string;
        role: string;
        repo?: string;
      }>;
      expect(body).toHaveLength(1);
      expect(body[0].sessionId).toBe("ses_aabbccddee01ABCDEFghijklmn");
      expect(body[0].role).toBe("legion-po");
      expect(body[0].repo).toBe("sjawhar/legion");
    });
  });

  describe("POST /promoted", () => {
    it("promotes a session", async () => {
      await startTestServer();
      const response = await requestJson("/promoted", {
        method: "POST",
        body: JSON.stringify({
          sessionId: "ses_aabbccddee01ABCDEFghijklmn",
          role: "legion-po",
          repo: "sjawhar/legion",
        }),
      });
      expect(response.status).toBe(201);
      const body = (await response.json()) as {
        sessionId: string;
        role: string;
        repo?: string;
        promotedAt: string;
      };
      expect(body.sessionId).toBe("ses_aabbccddee01ABCDEFghijklmn");
      expect(body.role).toBe("legion-po");
      expect(body.repo).toBe("sjawhar/legion");
      expect(body.promotedAt).toBeTruthy();

      // Verify it persisted
      const listResponse = await requestJson("/promoted");
      const list = (await listResponse.json()) as unknown[];
      expect(list).toHaveLength(1);
    });

    it("promotes without repo", async () => {
      await startTestServer();
      const response = await requestJson("/promoted", {
        method: "POST",
        body: JSON.stringify({
          sessionId: "ses_aabbccddee01ABCDEFghijklmn",
          role: "legion-po",
        }),
      });
      expect(response.status).toBe(201);
      const body = (await response.json()) as { repo?: string };
      expect(body.repo).toBeUndefined();
    });

    it("rejects missing sessionId", async () => {
      await startTestServer();
      const response = await requestJson("/promoted", {
        method: "POST",
        body: JSON.stringify({ role: "legion-po" }),
      });
      expect(response.status).toBe(400);
    });

    it("rejects missing role", async () => {
      await startTestServer();
      const response = await requestJson("/promoted", {
        method: "POST",
        body: JSON.stringify({ sessionId: "ses_aabbccddee01ABCDEFghijklmn" }),
      });
      expect(response.status).toBe(400);
    });

    it("rejects invalid session ID format", async () => {
      await startTestServer();
      const response = await requestJson("/promoted", {
        method: "POST",
        body: JSON.stringify({ sessionId: "bad-id", role: "legion-po" }),
      });
      expect(response.status).toBe(400);
      const body = (await response.json()) as { error: string };
      expect(body.error).toBe("invalid_session_id");
    });

    it("rejects invalid JSON", async () => {
      await startTestServer();
      const response = await fetch(`${baseUrl}/promoted`, {
        method: "POST",
        headers: { "content-type": "application/json" },
        body: "not json",
      });
      expect(response.status).toBe(400);
    });
  });

  describe("DELETE /promoted/:sessionId", () => {
    it("demotes an existing session", async () => {
      await startTestServer();
      // First promote
      await requestJson("/promoted", {
        method: "POST",
        body: JSON.stringify({
          sessionId: "ses_aabbccddee01ABCDEFghijklmn",
          role: "legion-po",
        }),
      });

      // Then demote
      const response = await requestJson("/promoted/ses_aabbccddee01ABCDEFghijklmn", {
        method: "DELETE",
      });
      expect(response.status).toBe(200);
      const body = (await response.json()) as { demoted: string };
      expect(body.demoted).toBe("ses_aabbccddee01ABCDEFghijklmn");

      // Verify it's gone
      const listResponse = await requestJson("/promoted");
      const list = (await listResponse.json()) as unknown[];
      expect(list).toHaveLength(0);
    });

    it("returns 404 for non-existent session", async () => {
      await startTestServer();
      const response = await requestJson("/promoted/ses_aabbccddee01ABCDEFghijklmn", {
        method: "DELETE",
      });
      expect(response.status).toBe(404);
    });
  });
});

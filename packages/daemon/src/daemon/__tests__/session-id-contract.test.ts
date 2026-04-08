import { afterEach, describe, expect, it } from "bun:test";
import { mkdtemp, rm } from "node:fs/promises";
import os from "node:os";
import path from "node:path";
import { computeSessionId } from "../../state/types";
import type { RuntimeAdapter } from "../runtime/types";
import { startServer } from "../server";

describe("sessionId contract (daemon vs state)", () => {
  let tempDir: string | null = null;
  let stopServer: (() => void) | null = null;

  const originalFetch = globalThis.fetch;
  const legionId = "123e4567-e89b-12d3-a456-426614174000";

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
    const createSessionCalls: Array<{ sessionId: string; workspace: string }> = [];
    const sharedServePort = 16500;
    const adapter: RuntimeAdapter = {
      start: async () => {},
      stop: async () => {},
      healthy: async () => true,
      getPort: () => sharedServePort,
      getServePid: () => 0,
      createSession: async (sessionId: string, workspace: string) => {
        createSessionCalls.push({ sessionId, workspace });
        return sessionId;
      },
      sendPrompt: async () => {},
      getSessionStatus: async () => ({ data: undefined }),
      deleteSession: async () => {},
    };

    tempDir = await mkdtemp(path.join(os.tmpdir(), "legion-session-contract-"));
    const stateFilePath = path.join(tempDir, "workers.json");
    const { server, stop } = startServer({
      port: 0,
      hostname: "127.0.0.1",
      legionId,
      legionDir: tempDir,
      adapter,
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
    expect(body.sessionId).toBe(computeSessionId(legionId, "ENG-42", "implement"));
    expect(createSessionCalls[0].sessionId).toBe(computeSessionId(legionId, "ENG-42", "implement"));
  });

  it("threads version to deterministic sessionId contract", async () => {
    const createSessionCalls: Array<{ sessionId: string; workspace: string }> = [];
    const sharedServePort = 16500;
    const adapter: RuntimeAdapter = {
      start: async () => {},
      stop: async () => {},
      healthy: async () => true,
      getPort: () => sharedServePort,
      getServePid: () => 0,
      createSession: async (sessionId: string, workspace: string) => {
        createSessionCalls.push({ sessionId, workspace });
        return sessionId;
      },
      sendPrompt: async () => {},
      getSessionStatus: async () => ({ data: undefined }),
      deleteSession: async () => {},
    };

    tempDir = await mkdtemp(path.join(os.tmpdir(), "legion-session-contract-"));
    const stateFilePath = path.join(tempDir, "workers.json");
    const { server, stop } = startServer({
      port: 0,
      hostname: "127.0.0.1",
      legionId,
      legionDir: tempDir,
      adapter,
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
        version: 2,
      }),
    });

    expect(response.status).toBe(200);
    const body = (await response.json()) as { id: string; sessionId: string };
    expect(body.sessionId).toBe(computeSessionId(legionId, "ENG-42", "implement", 2));
    expect(createSessionCalls[0].sessionId).toBe(
      computeSessionId(legionId, "ENG-42", "implement", 2)
    );
  });

  it("stores actual session ID when adapter returns different ID than requested", async () => {
    const actualSessionId = "ses_actual_from_serve";
    const sendPromptCalls: Array<{ sessionId: string; text: string }> = [];
    const sharedServePort = 16500;
    const adapter: RuntimeAdapter = {
      start: async () => {},
      stop: async () => {},
      healthy: async () => true,
      getPort: () => sharedServePort,
      getServePid: () => 0,
      createSession: async (_sessionId: string, _workspace: string) => {
        // Simulate serve returning a different session ID (e.g., 409 recovery)
        return actualSessionId;
      },
      sendPrompt: async (sessionId: string, text: string) => {
        sendPromptCalls.push({ sessionId, text });
      },
      getSessionStatus: async () => ({ data: undefined }),
      deleteSession: async () => {},
    };

    tempDir = await mkdtemp(path.join(os.tmpdir(), "legion-session-contract-"));
    const stateFilePath = path.join(tempDir, "workers.json");
    const { server, stop } = startServer({
      port: 0,
      hostname: "127.0.0.1",
      legionId,
      legionDir: tempDir,
      adapter,
      stateFilePath,
    });
    stopServer = stop;

    const baseUrl = `http://127.0.0.1:${server.port}`;

    // Create worker — adapter returns different session ID
    const createRes = await originalFetch(`${baseUrl}/workers`, {
      method: "POST",
      headers: { "content-type": "application/json" },
      body: JSON.stringify({
        issueId: "ENG-99",
        mode: "implement",
        workspace: "/tmp/work",
      }),
    });

    expect(createRes.status).toBe(200);
    const createBody = (await createRes.json()) as { id: string; sessionId: string };
    expect(createBody.sessionId).toBe(actualSessionId);

    // Send prompt — should use actual session ID, not computed
    const promptRes = await originalFetch(`${baseUrl}/workers/eng-99-implement/prompt`, {
      method: "POST",
      headers: { "content-type": "application/json" },
      body: JSON.stringify({ text: "hello worker" }),
    });

    expect(promptRes.status).toBe(200);
    expect(sendPromptCalls).toHaveLength(1);
    expect(sendPromptCalls[0].sessionId).toBe(actualSessionId);
    expect(sendPromptCalls[0].text).toBe("hello worker");
  });
});

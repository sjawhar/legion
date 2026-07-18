import { afterEach, beforeEach, describe, expect, it } from "bun:test";
import { mkdtemp, rm } from "node:fs/promises";
import os from "node:os";
import path from "node:path";
import type { RuntimeAdapter } from "../runtime/types";
import { startServer } from "../server";
import { createMockEnvoyServer, type MockEnvoyServer } from "./mock-envoy-server";

const sharedServePort = 15500;

describe("POST /state/advance", () => {
  let tempDir: string | null = null;
  let stopServer: (() => void) | null = null;
  let baseUrl = "";
  let createSessionCalls: Array<{ sessionId: string; workspace: string }> = [];
  let deleteSessionCalls: string[] = [];
  const originalFetch = globalThis.fetch;
  const legionId = "test-org/1";
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
      createSession: async (sessionId: string, workspace: string) => {
        createSessionCalls.push({ sessionId, workspace });
        return sessionId;
      },
      sendPrompt: async () => {},
      getSessionStatus: async () => ({ data: undefined }),
      deleteSession: async (sessionId: string) => {
        deleteSessionCalls.push(sessionId);
      },
      sessionExists: async () => false,
    };
  }

  function makePaths(tmpDir: string) {
    return {
      dataDir: tmpDir,
      stateDir: tmpDir,
      reposDir: path.join(tmpDir, "repos"),
      workspacesDir: path.join(tmpDir, "workspaces"),
      legionsFile: path.join(tmpDir, "legions.json"),
      forLegion: (id: string) => ({
        legionStateDir: path.join(tmpDir, id),
        workersFile: path.join(tmpDir, id, "workers.json"),
        promotedFile: path.join(tmpDir, id, "promoted.json"),
        feedbackFile: path.join(tmpDir, id, "feedback.jsonl"),
        logDir: path.join(tmpDir, id, "logs"),
        workspacesDir: path.join(tmpDir, "workspaces", id),
      }),
      repoClonePath: (host: string, owner: string, repo: string) =>
        path.join(tmpDir, "repos", host, `${owner}-${repo}`),
    };
  }

  function makeRepoManagerDeps(_tmpDir: string) {
    return {
      exists: async () => true,
      runJj: async () => ({ exitCode: 0, stdout: "", stderr: "" }),
      rmDir: async () => {},
      symlink: async () => {},
      listDir: async () => [] as string[],
    };
  }

  async function startTestServer(options?: { issueBackend?: "linear" | "github" }) {
    createSessionCalls = [];
    deleteSessionCalls = [];
    const adapter = makeAdapter();
    tempDir = await mkdtemp(path.join(os.tmpdir(), "legion-advance-"));
    const stateFilePath = path.join(tempDir, "workers.json");
    const { server, stop } = startServer({
      port: 0,
      hostname: "127.0.0.1",
      envoyUrl: mockEnvoy.url,
      legionId,
      legionDir: tempDir,
      paths: makePaths(tempDir),
      adapter,
      repoManagerDeps: makeRepoManagerDeps(tempDir),
      stateFilePath,
      issueBackend: options?.issueBackend ?? "github",
    });
    stopServer = stop;
    baseUrl = `http://127.0.0.1:${server.port}`;
  }

  async function requestJson(pathname: string, init?: RequestInit) {
    return originalFetch(`${baseUrl}${pathname}`, {
      ...init,
      headers: {
        "content-type": "application/json",
        ...(init?.headers ?? {}),
      },
    });
  }

  /**
   * Seed the issue state cache by posting crafted GitHub project items
   * through POST /state/collect.
   */
  async function seedIssueInCache(
    owner: string,
    repo: string,
    issueNumber: number,
    status: string,
    labels: string[] = [],
    prUrl?: string
  ) {
    const linkedPRs = prUrl ? [prUrl] : [];
    const item = {
      content: {
        number: issueNumber,
        repository: `${owner}/${repo}`,
        url: `https://github.com/${owner}/${repo}/issues/${issueNumber}`,
        type: "Issue",
      },
      status,
      labels,
      "linked pull requests": linkedPRs,
    };
    const response = await requestJson("/state/collect", {
      method: "POST",
      body: JSON.stringify({ backend: "github", issues: [item] }),
    });
    expect(response.status).toBe(200);
  }

  afterEach(async () => {
    stopServer?.();
    stopServer = null;
    if (tempDir) {
      await rm(tempDir, { recursive: true, force: true }).catch(() => {});
      tempDir = null;
    }
    mockEnvoy.stop();
  });

  it("returns 400 when issueId is missing", async () => {
    await startTestServer();
    const response = await requestJson("/state/advance", {
      method: "POST",
      body: JSON.stringify({}),
    });
    expect(response.status).toBe(400);
    const body = (await response.json()) as { error: string };
    expect(body.error).toBe("missing_issue_id");
  });

  it("returns 412 when issue not in state cache", async () => {
    await startTestServer();
    const response = await requestJson("/state/advance", {
      method: "POST",
      body: JSON.stringify({ issueId: "unknown-issue-99" }),
    });
    expect(response.status).toBe(412);
    const body = (await response.json()) as { error: string };
    expect(body.error).toBe("issue_not_in_cache");
  });

  it("does not dispatch without explicit /workers or /state/advance", async () => {
    await startTestServer();

    await seedIssueInCache("acme", "backend", 42, "Todo");

    expect(createSessionCalls).toHaveLength(0);
  });

  it("returns skipped for skip action (Triage status)", async () => {
    await startTestServer();
    await seedIssueInCache("acme", "backend", 42, "Triage");

    const response = await requestJson("/state/advance", {
      method: "POST",
      body: JSON.stringify({ issueId: "acme-backend-42" }),
    });
    expect(response.status).toBe(200);
    const body = (await response.json()) as {
      action: string;
      executed: string;
    };
    expect(body.executed).toBe("skipped");
    expect(body.action).toBe("skip");
  });

  it("dispatches worker for dispatch_planner action (Todo status)", async () => {
    await startTestServer();
    await seedIssueInCache("acme", "backend", 42, "Todo");

    const response = await requestJson("/state/advance", {
      method: "POST",
      body: JSON.stringify({ issueId: "acme-backend-42" }),
    });
    expect(response.status).toBe(200);
    const body = (await response.json()) as {
      action: string;
      executed: string;
      workerId?: string;
    };
    expect(body.executed).toBe("dispatched");
    expect(body.action).toBe("dispatch_planner");
    expect(body.workerId).toBe("acme-backend-42-plan");
  });

  it("dispatches worker for dispatch_implementer action (In Progress status)", async () => {
    await startTestServer();
    await seedIssueInCache("acme", "backend", 42, "In Progress");

    const response = await requestJson("/state/advance", {
      method: "POST",
      body: JSON.stringify({ issueId: "acme-backend-42" }),
    });
    expect(response.status).toBe(200);
    const body = (await response.json()) as {
      action: string;
      executed: string;
      workerId?: string;
    };
    expect(body.executed).toBe("dispatched");
    expect(body.action).toBe("dispatch_implementer");
    expect(body.workerId).toBe("acme-backend-42-implement");
  });

  it("dispatches architect for Backlog status without worker-done", async () => {
    await startTestServer();
    await seedIssueInCache("acme", "backend", 42, "Backlog");

    const response = await requestJson("/state/advance", {
      method: "POST",
      body: JSON.stringify({ issueId: "acme-backend-42" }),
    });
    expect(response.status).toBe(200);
    const body = (await response.json()) as {
      action: string;
      executed: string;
      workerId?: string;
    };
    expect(body.executed).toBe("dispatched");
    expect(body.action).toBe("dispatch_architect");
    expect(body.workerId).toBe("acme-backend-42-architect");
  });

  it("forces dispatch of specified stage with --stage", async () => {
    await startTestServer();
    // Seed in Triage (would normally skip)
    await seedIssueInCache("acme", "backend", 42, "Triage");

    const response = await requestJson("/state/advance", {
      method: "POST",
      body: JSON.stringify({ issueId: "acme-backend-42", stage: "implement" }),
    });
    expect(response.status).toBe(200);
    const body = (await response.json()) as {
      action: string;
      executed: string;
      workerId?: string;
    };
    expect(body.executed).toBe("dispatched");
    expect(body.workerId).toBe("acme-backend-42-implement");
  });

  it("returns 409 when worker is already running (via 409 from POST /workers)", async () => {
    await startTestServer();
    // Seed a Todo issue so it suggests dispatch_planner
    await seedIssueInCache("acme", "backend", 42, "Todo");

    // Dispatch a worker first
    const first = await requestJson("/state/advance", {
      method: "POST",
      body: JSON.stringify({ issueId: "acme-backend-42" }),
    });
    expect(first.status).toBe(200);

    // Second advance should hit 409 from POST /workers
    const second = await requestJson("/state/advance", {
      method: "POST",
      body: JSON.stringify({ issueId: "acme-backend-42" }),
    });
    // Either 409 from advance checking hasLiveWorker, or skip (since cache doesn't refresh between calls).
    // Since the cached state still says hasLiveWorker=false (it was seeded via /state/collect before
    // the worker was dispatched), the advance will try POST /workers which returns 409.
    expect(second.status).toBe(409);
  });
  it("POST /state/auto-advance returns 404", async () => {
    await startTestServer();

    const response = await requestJson("/state/auto-advance", { method: "POST" });

    expect(response.status).toBe(404);
  });
});

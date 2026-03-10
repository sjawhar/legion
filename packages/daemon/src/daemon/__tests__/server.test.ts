import { afterEach, describe, expect, it } from "bun:test";
import { mkdtemp, readFile, rm } from "node:fs/promises";
import os from "node:os";
import path from "node:path";
import { computeSessionId } from "../../state/types";
import type { LegionPaths } from "../paths";
import type { RepoManagerDeps } from "../repo-manager";
import type { RuntimeAdapter } from "../runtime/types";
import type { WorkerEntry } from "../serve-manager";
import { startServer } from "../server";
import { type PersistedWorkerState, writeStateFile } from "../state-file";

const sharedServePort = 15500;

describe("daemon server", () => {
  let tempDir: string | null = null;
  let stopServer: (() => void) | null = null;
  let baseUrl = "";
  let createSessionCalls: Array<{ sessionId: string; workspace: string }> = [];
  let sessionStatusHandler:
    | ((sessionId: string) => Promise<{ data?: unknown; error?: unknown }>)
    | null = null;
  const originalFetch = globalThis.fetch;
  const legionId = "123e4567-e89b-12d3-a456-426614174000";

  function makeAdapter(): RuntimeAdapter {
    return {
      start: async () => {},
      stop: async () => {},
      healthy: async () => true,
      getPort: () => sharedServePort,
      createSession: async (sessionId: string, workspace: string) => {
        createSessionCalls.push({ sessionId, workspace });
        return sessionId;
      },
      sendPrompt: async () => {},
      getSessionStatus: async (sessionId: string) => {
        if (sessionStatusHandler) {
          return sessionStatusHandler(sessionId);
        }
        return { data: undefined };
      },
    };
  }

  async function startTestServer(options?: {
    state?: PersistedWorkerState;
    adapterOverrides?: Partial<RuntimeAdapter>;
    paths?: LegionPaths;
    repoManagerDeps?: RepoManagerDeps;
    runtime?: string;
    tmuxSession?: string;
  }) {
    createSessionCalls = [];
    let adapter = makeAdapter();
    if (options?.adapterOverrides) {
      adapter = { ...adapter, ...options.adapterOverrides };
    }
    tempDir = await mkdtemp(path.join(os.tmpdir(), "legion-server-"));
    const stateFilePath = path.join(tempDir, "workers.json");
    if (options?.state) {
      await writeStateFile(stateFilePath, options.state);
    }
    const { server, stop } = startServer({
      port: 0,
      hostname: "127.0.0.1",
      legionId,
      legionDir: tempDir,
      paths: options?.paths,
      adapter,
      repoManagerDeps: options?.repoManagerDeps,
      stateFilePath,
      runtime: options?.runtime,
      tmuxSession: options?.tmuxSession,
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
    sessionStatusHandler = null;
    if (stopServer) {
      stopServer();
      stopServer = null;
    }
    if (tempDir) {
      await rm(tempDir, { recursive: true, force: true });
      tempDir = null;
    }
  });

  it("returns health data with default runtime", async () => {
    await startTestServer();
    const response = await requestJson("/health");
    expect(response.status).toBe(200);
    const body = (await response.json()) as {
      status: string;
      uptime: number;
      workerCount: number;
      runtime: string;
    };
    expect(body.status).toBe("ok");
    expect(typeof body.uptime).toBe("number");
    expect(body.workerCount).toBe(0);
    expect(body.runtime).toBe("opencode");
  });

  it("returns health data with configured runtime and tmuxSession", async () => {
    await startTestServer({ runtime: "claude-code", tmuxSession: "legion-abc123" });
    const response = await requestJson("/health");
    expect(response.status).toBe(200);
    const body = (await response.json()) as {
      status: string;
      runtime: string;
      tmuxSession?: string;
    };
    expect(body.runtime).toBe("claude-code");
    expect(body.tmuxSession).toBe("legion-abc123");
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

  it("rejects worker creation when both repo and workspace are missing", async () => {
    await startTestServer();
    const response = await requestJson("/workers", {
      method: "POST",
      body: JSON.stringify({
        issueId: "ENG-50",
        mode: "implement",
      }),
    });
    expect(response.status).toBe(400);
    const body = (await response.json()) as { error: string };
    expect(body.error).toBe("missing repo or workspace");
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
    expect(body.sessionId).toBe(computeSessionId(legionId, "eng-42", "implement"));

    expect(createSessionCalls.length).toBe(1);
    expect(createSessionCalls[0].sessionId).toBe(computeSessionId(legionId, "eng-42", "implement"));
    expect(createSessionCalls[0].workspace).toBe("/tmp/work");

    const listResponse = await requestJson("/workers");
    const listBody = (await listResponse.json()) as WorkerEntry[];
    expect(listBody.length).toBe(1);

    const entryResponse = await requestJson(`/workers/${body.id}`);
    expect(entryResponse.status).toBe(200);
    const entryBody = (await entryResponse.json()) as WorkerEntry;
    expect(entryBody.port).toBe(sharedServePort);
  });

  it("creates workers from repo by resolving workspace path", async () => {
    const paths: LegionPaths = {
      dataDir: "/tmp/legion-data",
      stateDir: "/tmp/legion-state",
      reposDir: "/tmp/legion-data/repos",
      workspacesDir: "/tmp/legion-data/workspaces",
      legionsFile: "/tmp/legion-state/legions.json",
      forLegion: (projectId: string) => ({
        legionStateDir: `/tmp/legion-state/legions/${projectId}`,
        workersFile: `/tmp/legion-state/legions/${projectId}/workers.json`,
        logDir: `/tmp/legion-state/legions/${projectId}/logs`,
        workspacesDir: `/tmp/legion-data/workspaces/${projectId}`,
      }),
      repoClonePath: (host: string, owner: string, repo: string) =>
        `/tmp/legion-data/repos/${host}/${owner}/${repo}`,
    };
    const runJjCalls: string[][] = [];
    const repoManagerDeps: RepoManagerDeps = {
      runJj: async (args: string[]) => {
        runJjCalls.push(args);
        return { exitCode: 0, stdout: "", stderr: "" };
      },
      exists: async () => false,
      rmDir: async () => {},
    };
    await startTestServer({ paths, repoManagerDeps });

    const response = await requestJson("/workers", {
      method: "POST",
      body: JSON.stringify({
        issueId: "acme-widgets-77",
        mode: "implement",
        repo: "acme/widgets",
      }),
    });

    expect(response.status).toBe(200);
    expect(createSessionCalls.length).toBe(1);
    expect(createSessionCalls[0].workspace).toBe(
      "/tmp/legion-data/workspaces/123e4567-e89b-12d3-a456-426614174000/acme-widgets-77"
    );
    expect(runJjCalls).toEqual([
      [
        "git",
        "clone",
        "https://github.com/acme/widgets",
        "/tmp/legion-data/repos/github.com/acme/widgets",
      ],
      [
        "workspace",
        "add",
        "/tmp/legion-data/workspaces/123e4567-e89b-12d3-a456-426614174000/acme-widgets-77",
        "--name",
        "acme-widgets-77",
        "-R",
        "/tmp/legion-data/repos/github.com/acme/widgets",
      ],
    ]);
  });

  it("creates workers from legacy workspace payload", async () => {
    await startTestServer();
    const response = await requestJson("/workers", {
      method: "POST",
      body: JSON.stringify({
        issueId: "ENG-78",
        mode: "implement",
        workspace: "/tmp/legacy-workspace",
      }),
    });

    expect(response.status).toBe(200);
    expect(createSessionCalls.length).toBe(1);
    expect(createSessionCalls[0].workspace).toBe("/tmp/legacy-workspace");
  });

  describe("characterization: POST /workers routing", () => {
    it("with only repo resolves workspace and creates worker", async () => {
      const paths: LegionPaths = {
        dataDir: "/tmp/legion-data",
        stateDir: "/tmp/legion-state",
        reposDir: "/tmp/legion-data/repos",
        workspacesDir: "/tmp/legion-data/workspaces",
        legionsFile: "/tmp/legion-state/legions.json",
        forLegion: (projectId: string) => ({
          legionStateDir: `/tmp/legion-state/legions/${projectId}`,
          workersFile: `/tmp/legion-state/legions/${projectId}/workers.json`,
          logDir: `/tmp/legion-state/legions/${projectId}/logs`,
          workspacesDir: `/tmp/legion-data/workspaces/${projectId}`,
        }),
        repoClonePath: (host: string, owner: string, repo: string) =>
          `/tmp/legion-data/repos/${host}/${owner}/${repo}`,
      };
      const repoManagerDeps: RepoManagerDeps = {
        runJj: async () => ({ exitCode: 0, stdout: "", stderr: "" }),
        exists: async () => false,
        rmDir: async () => {},
      };
      await startTestServer({ paths, repoManagerDeps });

      const response = await requestJson("/workers", {
        method: "POST",
        body: JSON.stringify({
          issueId: "acme-widgets-80",
          mode: "implement",
          repo: "acme/widgets",
        }),
      });

      expect(response.status).toBe(200);
      expect(createSessionCalls).toHaveLength(1);
      expect(createSessionCalls[0].workspace).toBe(
        "/tmp/legion-data/workspaces/123e4567-e89b-12d3-a456-426614174000/acme-widgets-80"
      );
    });

    it("with only workspace uses it directly", async () => {
      await startTestServer();

      const response = await requestJson("/workers", {
        method: "POST",
        body: JSON.stringify({
          issueId: "ENG-80",
          mode: "implement",
          workspace: "/tmp/characterization-workspace",
        }),
      });

      expect(response.status).toBe(200);
      expect(createSessionCalls).toHaveLength(1);
      expect(createSessionCalls[0].workspace).toBe("/tmp/characterization-workspace");
    });

    it("with neither repo nor workspace returns 400", async () => {
      await startTestServer();

      const response = await requestJson("/workers", {
        method: "POST",
        body: JSON.stringify({ issueId: "ENG-81", mode: "implement" }),
      });

      expect(response.status).toBe(400);
      expect(await response.json()).toEqual({ error: "missing repo or workspace" });
    });
  });

  it("returns 500 when repo fetch fails", async () => {
    const paths: LegionPaths = {
      dataDir: "/tmp/legion-data",
      stateDir: "/tmp/legion-state",
      reposDir: "/tmp/legion-data/repos",
      workspacesDir: "/tmp/legion-data/workspaces",
      legionsFile: "/tmp/legion-state/legions.json",
      forLegion: (projectId: string) => ({
        legionStateDir: `/tmp/legion-state/legions/${projectId}`,
        workersFile: `/tmp/legion-state/legions/${projectId}/workers.json`,
        logDir: `/tmp/legion-state/legions/${projectId}/logs`,
        workspacesDir: `/tmp/legion-data/workspaces/${projectId}`,
      }),
      repoClonePath: (host: string, owner: string, repo: string) =>
        `/tmp/legion-data/repos/${host}/${owner}/${repo}`,
    };
    const repoManagerDeps: RepoManagerDeps = {
      runJj: async () => ({ exitCode: 128, stdout: "", stderr: "Permission denied (publickey)" }),
      exists: async () => true,
      rmDir: async () => {},
    };
    await startTestServer({ paths, repoManagerDeps });

    const response = await requestJson("/workers", {
      method: "POST",
      body: JSON.stringify({
        issueId: "acme-widgets-99",
        mode: "implement",
        repo: "acme/widgets",
      }),
    });

    expect(response.status).toBe(500);
    const body = (await response.json()) as { error: string };
    expect(body.error).toContain("Failed to resolve workspace");
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
      sessionId: computeSessionId(legionId, "eng-1", "implement"),
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

  it("cleans worker workspace by repo", async () => {
    const paths: LegionPaths = {
      dataDir: "/tmp/legion-data",
      stateDir: "/tmp/legion-state",
      reposDir: "/tmp/legion-data/repos",
      workspacesDir: "/tmp/legion-data/workspaces",
      legionsFile: "/tmp/legion-state/legions.json",
      forLegion: (projectId: string) => ({
        legionStateDir: `/tmp/legion-state/legions/${projectId}`,
        workersFile: `/tmp/legion-state/legions/${projectId}/workers.json`,
        logDir: `/tmp/legion-state/legions/${projectId}/logs`,
        workspacesDir: `/tmp/legion-data/workspaces/${projectId}`,
      }),
      repoClonePath: (host: string, owner: string, repo: string) =>
        `/tmp/legion-data/repos/${host}/${owner}/${repo}`,
    };
    const runJjCalls: string[][] = [];
    const rmDirCalls: string[] = [];
    const repoManagerDeps: RepoManagerDeps = {
      runJj: async (args: string[]) => {
        runJjCalls.push(args);
        return { exitCode: 0, stdout: "", stderr: "" };
      },
      exists: async () => true,
      rmDir: async (workspacePath: string) => {
        rmDirCalls.push(workspacePath);
      },
    };
    await startTestServer({ paths, repoManagerDeps });

    const createResponse = await requestJson("/workers", {
      method: "POST",
      body: JSON.stringify({
        issueId: "ENG-79",
        mode: "implement",
        workspace: "/tmp/worker-79",
      }),
    });
    const created = (await createResponse.json()) as { id: string };

    const cleanupResponse = await requestJson(`/workers/${created.id}/workspace`, {
      method: "DELETE",
      body: JSON.stringify({ repo: "acme/widgets" }),
    });

    expect(cleanupResponse.status).toBe(200);
    expect(await cleanupResponse.json()).toEqual({ status: "cleaned" });
    expect(runJjCalls).toEqual([
      ["workspace", "forget", "eng-79", "-R", "/tmp/legion-data/repos/github.com/acme/widgets"],
    ]);
    expect(rmDirCalls).toEqual([
      "/tmp/legion-data/workspaces/123e4567-e89b-12d3-a456-426614174000/eng-79",
    ]);
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

    sessionStatusHandler = async () => ({
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

    sessionStatusHandler = async () => {
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

    sessionStatusHandler = async () => ({
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

  describe("POST /workers/:id/prompt", () => {
    const baseWorkerEntry: WorkerEntry = {
      id: "leg-42-implement",
      port: sharedServePort,
      sessionId: computeSessionId(legionId, "leg-42", "implement"),
      workspace: "/tmp/work",
      startedAt: "2026-02-01T00:00:00.000Z",
      status: "running",
      crashCount: 0,
      lastCrashAt: null,
    };

    it("sends prompt to worker via adapter", async () => {
      const sendPromptCalls: Array<{ sessionId: string; text: string }> = [];
      await startTestServer({
        state: {
          workers: { "leg-42-implement": { ...baseWorkerEntry, id: "leg-42-implement" } },
          crashHistory: {},
        },
        adapterOverrides: {
          sendPrompt: async (sessionId, text) => {
            sendPromptCalls.push({ sessionId, text });
          },
        },
      });

      const response = await requestJson("/workers/leg-42-implement/prompt", {
        method: "POST",
        body: JSON.stringify({ text: "hello world" }),
      });

      expect(response.status).toBe(200);
      const body = (await response.json()) as { ok: boolean };
      expect(body.ok).toBe(true);
      expect(sendPromptCalls).toHaveLength(1);
      expect(sendPromptCalls[0].sessionId).toBe(baseWorkerEntry.sessionId);
      expect(sendPromptCalls[0].text).toBe("hello world");
    });

    it("returns 404 for unknown worker prompt", async () => {
      await startTestServer();
      const response = await requestJson("/workers/unknown/prompt", {
        method: "POST",
        body: JSON.stringify({ text: "hello" }),
      });
      expect(response.status).toBe(404);
    });

    it("returns 400 for missing text in prompt", async () => {
      await startTestServer({
        state: {
          workers: { "leg-42-implement": { ...baseWorkerEntry, id: "leg-42-implement" } },
          crashHistory: {},
        },
      });
      const response = await requestJson("/workers/leg-42-implement/prompt", {
        method: "POST",
        body: JSON.stringify({ notText: "hello" }),
      });
      expect(response.status).toBe(400);
    });

    it("returns 500 when adapter.sendPrompt fails", async () => {
      await startTestServer({
        state: {
          workers: { "leg-42-implement": { ...baseWorkerEntry, id: "leg-42-implement" } },
          crashHistory: {},
        },
        adapterOverrides: {
          sendPrompt: async () => {
            throw new Error("session not found");
          },
        },
      });
      const response = await requestJson("/workers/leg-42-implement/prompt", {
        method: "POST",
        body: JSON.stringify({ text: "hello" }),
      });
      expect(response.status).toBe(500);
      const body = (await response.json()) as { error: string };
      expect(body.error).toContain("Failed to send prompt");
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
      legionId,
      legionDir: tempDir ?? os.tmpdir(),
      adapter: makeAdapter(),
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

  describe("POST /state/fetch-and-collect", () => {
    it("rejects invalid backend", async () => {
      await startTestServer();
      const response = await requestJson("/state/fetch-and-collect", {
        method: "POST",
        body: JSON.stringify({ backend: "invalid-backend", legionId: "team-123" }),
      });
      expect(response.status).toBe(400);
      const body = (await response.json()) as { error: string };
      expect(body.error).toBe("invalid_backend");
    });

    it("rejects linear backend with github-only message", async () => {
      await startTestServer();
      const response = await requestJson("/state/fetch-and-collect", {
        method: "POST",
        body: JSON.stringify({ backend: "linear", legionId: "team-123" }),
      });
      expect(response.status).toBe(400);
      const body = (await response.json()) as { error: string };
      expect(body.error).toBe("fetch-and-collect only supports github backend currently");
    });

    it("rejects missing backend field", async () => {
      await startTestServer();
      const response = await requestJson("/state/fetch-and-collect", {
        method: "POST",
        body: JSON.stringify({ legionId: "owner/123" }),
      });
      expect(response.status).toBe(400);
      const body = (await response.json()) as { error: string };
      expect(body.error).toBe("invalid_backend");
    });

    it("rejects invalid JSON body", async () => {
      await startTestServer();
      const response = await requestJson("/state/fetch-and-collect", {
        method: "POST",
        body: "not-json",
      });
      expect(response.status).toBe(400);
      const body = (await response.json()) as { error: string };
      expect(body.error).toBe("invalid_json");
    });

    it("rejects github backend with invalid legionId format", async () => {
      await startTestServer();
      const response = await requestJson("/state/fetch-and-collect", {
        method: "POST",
        body: JSON.stringify({ backend: "github", legionId: "no-slash" }),
      });
      expect(response.status).toBe(400);
      const body = (await response.json()) as { error: string };
      expect(body.error).toBe("invalid_team_id: expected owner/project-number");
    });

    it("rejects github backend with non-numeric project number", async () => {
      await startTestServer();
      const response = await requestJson("/state/fetch-and-collect", {
        method: "POST",
        body: JSON.stringify({ backend: "github", legionId: "owner/abc" }),
      });
      expect(response.status).toBe(400);
      const body = (await response.json()) as { error: string };
      expect(body.error).toBe("invalid_team_id: project number not a number");
    });

    it("returns 500 when github fetch fails (no real API)", async () => {
      await startTestServer();
      const response = await requestJson("/state/fetch-and-collect", {
        method: "POST",
        body: JSON.stringify({ backend: "github", legionId: "owner/123" }),
      });
      expect(response.status).toBe(500);
      const body = (await response.json()) as { error: string };
      expect(body.error).toContain("fetch_and_collect_failed");
    });
  });

  describe("persistState serialization", () => {
    it("concurrent state writes produce valid JSON", async () => {
      await startTestServer();

      // Register multiple workers concurrently to trigger parallel persistState calls
      const concurrentRequests = Array.from({ length: 5 }, (_, i) =>
        requestJson("/workers", {
          method: "POST",
          body: JSON.stringify({
            issueId: `SERIAL-${i}`,
            mode: "implement",
            workspace: `/tmp/ws-${i}`,
          }),
        })
      );

      const responses = await Promise.all(concurrentRequests);
      for (const response of responses) {
        expect(response.status).toBe(200);
      }

      // Give a tick for any trailing async writes to complete
      await new Promise((r) => setTimeout(r, 50));

      // Read the state file directly and verify it's valid JSON
      const raw = await readFile(path.join(tempDir ?? os.tmpdir(), "workers.json"), "utf-8");
      const parsed = JSON.parse(raw) as PersistedWorkerState;
      expect(parsed).toBeDefined();
      expect(typeof parsed.workers).toBe("object");

      // All 5 workers should be present in the final state
      const workerIds = Object.keys(parsed.workers);
      expect(workerIds.length).toBe(5);
    });
  });
});

import { afterEach, beforeEach, describe, expect, it } from "bun:test";
import { mkdir, mkdtemp, readFile, rm } from "node:fs/promises";
import os from "node:os";
import path from "node:path";
import { computeSessionId } from "../../state/types";
import type { GitHubAppsConfig } from "../config";
import { type FeedbackEvent, FeedbackLogger, type FeedbackWriter } from "../feedback";
import { TokenManager } from "../github-apps";
import type { LegionPaths } from "../paths";
import type { RepoManagerDeps } from "../repo-manager";
import type { RuntimeAdapter } from "../runtime/types";
import type { WorkerEntry } from "../serve-manager";
import { startServer } from "../server";
import { type PersistedWorkerState, writeStateFile } from "../state-file";
import { createMockEnvoyServer, type MockEnvoyServer } from "./mock-envoy-server";

const sharedServePort = 15500;

describe("daemon server", () => {
  let tempDir: string | null = null;
  let stopServer: (() => void) | null = null;
  let baseUrl = "";
  let serverFetchAndProcessState: (() => Promise<void>) | null = null;
  let createSessionCalls: Array<{ sessionId: string; workspace: string }> = [];
  let deleteSessionCalls: string[] = [];
  let sessionStatusHandler:
    | ((sessionId: string) => Promise<{ data?: unknown; error?: unknown }>)
    | null = null;
  const originalSpawn = Bun.spawn;
  const originalFetch = globalThis.fetch;
  const originalConsoleWarn = console.warn;
  const legionId = "123e4567-e89b-12d3-a456-426614174000";
  let mockEnvoy: MockEnvoyServer;

  beforeEach(() => {
    mockEnvoy = createMockEnvoyServer();
  });

  class RecordingFeedbackWriter implements FeedbackWriter {
    lines: string[] = [];

    async append(line: string): Promise<void> {
      this.lines.push(line);
    }

    async flush(): Promise<void> {}
  }

  function parseFeedbackLines(lines: string[]): FeedbackEvent[] {
    return lines.map((line) => JSON.parse(line) as FeedbackEvent);
  }

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
      getSessionStatus: async (sessionId: string) => {
        if (sessionStatusHandler) {
          return sessionStatusHandler(sessionId);
        }
        return { data: undefined };
      },
      deleteSession: async (sessionId: string) => {
        deleteSessionCalls.push(sessionId);
      },
      sessionExists: async () => false,
    };
  }

  async function startTestServer(options?: {
    state?: PersistedWorkerState;
    adapterOverrides?: Partial<RuntimeAdapter>;
    paths?: LegionPaths;
    repoManagerDeps?: RepoManagerDeps;
    tokenManager?: TokenManager;
    runtime?: string;
    tmuxSession?: string;
    feedbackLogger?: FeedbackLogger;
    getControllerState?: () => { sessionId: string; port?: number } | undefined;
    legionId?: string;
    extraProjects?: string[];
    fetchProjectItems?: (owner: string, projectNumber: number) => Promise<unknown>;
    envoyUrl?: string;
  }) {
    createSessionCalls = [];
    deleteSessionCalls = [];
    let adapter = makeAdapter();
    if (options?.adapterOverrides) {
      adapter = { ...adapter, ...options.adapterOverrides };
    }
    tempDir = await mkdtemp(path.join(os.tmpdir(), "legion-server-"));
    const stateFilePath = path.join(tempDir, "workers.json");
    if (options?.state) {
      await writeStateFile(stateFilePath, options.state);
    }
    const { server, stop, fetchAndProcessState } = startServer({
      port: 0,
      hostname: "127.0.0.1",
      envoyUrl: options?.envoyUrl ?? mockEnvoy.url,
      legionId: options?.legionId ?? legionId,
      extraProjects: options?.extraProjects,
      legionDir: tempDir,
      paths: options?.paths,
      adapter,
      repoManagerDeps: options?.repoManagerDeps,
      tokenManager: options?.tokenManager,
      stateFilePath,
      runtime: options?.runtime,
      tmuxSession: options?.tmuxSession,
      feedbackLogger: options?.feedbackLogger,
      getControllerState: options?.getControllerState,
      fetchProjectItems: options?.fetchProjectItems,
    });
    serverFetchAndProcessState = fetchAndProcessState;
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

  function createGitHubProjectItem(overrides?: {
    number?: number;
    status?: string;
    labels?: string[];
  }) {
    const number = overrides?.number ?? 42;
    return {
      id: `PVTI_${number}`,
      content: {
        number,
        repository: "acme/widgets",
        url: `https://github.com/acme/widgets/issues/${number}`,
        type: "Issue" as const,
      },
      status: overrides?.status ?? "Todo",
      labels: overrides?.labels ?? [],
    };
  }

  function getFetchUrl(input: string | { href?: unknown; url?: unknown }) {
    if (typeof input === "string") {
      return input;
    }
    if (typeof input.href === "string") {
      return input.href;
    }
    if (typeof input.url === "string") {
      return input.url;
    }
    throw new Error("unsupported fetch input");
  }

  function createTestTokenManager(config: GitHubAppsConfig, token = "ghs_owner_token") {
    const manager = new TokenManager(config);
    manager.getToken = async (role, owner) => ({
      token: `${token}:${role}:${owner}`,
      expiresAt: "2099-01-01T00:00:00.000Z",
      gitIdentity: {
        name: `${role}-bot[bot]`,
        email: `${owner}+${role}@users.noreply.github.com`,
      },
    });
    return manager;
  }

  afterEach(async () => {
    Bun.spawn = originalSpawn;
    globalThis.fetch = originalFetch;
    console.warn = originalConsoleWarn;
    sessionStatusHandler = null;
    serverFetchAndProcessState = null;
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
    expect(body.error).toContain("missing_repo");
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

  it("returns 422 for invalid sessionId format", async () => {
    await startTestServer();
    const response = await requestJson("/workers", {
      method: "POST",
      body: JSON.stringify({
        issueId: "ENG-42",
        mode: "implement",
        workspace: "/tmp/work",
        sessionId: "invalid_format",
      }),
    });
    expect(response.status).toBe(422);
    const body = (await response.json()) as Record<string, unknown>;
    expect(body.error).toBe("invalid_session_id");
  });

  it("returns 422 for sessionId with wrong type", async () => {
    await startTestServer();
    const response = await requestJson("/workers", {
      method: "POST",
      body: JSON.stringify({
        issueId: "ENG-42",
        mode: "implement",
        workspace: "/tmp/work",
        sessionId: 12345,
      }),
    });
    expect(response.status).toBe(422);
    const body = (await response.json()) as Record<string, unknown>;
    expect(body.error).toBe("invalid_session_id");
  });

  it("returns 409 session_already_enlisted when sessionId is tracked by live worker", async () => {
    await startTestServer();
    const first = await requestJson("/workers", {
      method: "POST",
      body: JSON.stringify({
        issueId: "ENG-42",
        mode: "implement",
        workspace: "/tmp/work",
      }),
    });
    expect(first.status).toBe(200);
    const firstBody = (await first.json()) as { sessionId: string };

    const response = await requestJson("/workers", {
      method: "POST",
      body: JSON.stringify({
        issueId: "ENG-99",
        mode: "plan",
        workspace: "/tmp/work",
        sessionId: firstBody.sessionId,
      }),
    });
    expect(response.status).toBe(409);
    const body = (await response.json()) as Record<string, unknown>;
    expect(body.error).toBe("session_already_enlisted");
    expect(body.id).toBe("eng-42-implement");
  });

  it("allows enlistment when existing worker with same sessionId is dead", async () => {
    await startTestServer();
    const first = await requestJson("/workers", {
      method: "POST",
      body: JSON.stringify({
        issueId: "ENG-42",
        mode: "implement",
        workspace: "/tmp/work",
      }),
    });
    expect(first.status).toBe(200);
    const firstBody = (await first.json()) as { id: string; sessionId: string };

    await requestJson(`/workers/${firstBody.id}`, {
      method: "PATCH",
      body: JSON.stringify({ status: "dead" }),
    });

    const response = await requestJson("/workers", {
      method: "POST",
      body: JSON.stringify({
        issueId: "ENG-99",
        mode: "plan",
        workspace: "/tmp/work",
        sessionId: firstBody.sessionId,
      }),
    });
    expect(response.status).toBe(200);
  });

  it("uses provided sessionId instead of computing one", async () => {
    await startTestServer();
    const customSessionId = "ses_31617365bffeUEa4wPBVIL2LBI";
    const response = await requestJson("/workers", {
      method: "POST",
      body: JSON.stringify({
        issueId: "ENG-42",
        mode: "implement",
        workspace: "/tmp/work",
        sessionId: customSessionId,
      }),
    });
    expect(response.status).toBe(200);
    const body = (await response.json()) as { id: string; sessionId: string };
    expect(body.sessionId).toBe(customSessionId);
    expect(createSessionCalls[0].sessionId).toBe(customSessionId);
  });

  it("computes sessionId normally when sessionId not provided", async () => {
    await startTestServer();
    const response = await requestJson("/workers", {
      method: "POST",
      body: JSON.stringify({
        issueId: "ENG-42",
        mode: "implement",
        workspace: "/tmp/work",
      }),
    });
    expect(response.status).toBe(200);
    const body = (await response.json()) as { sessionId: string };
    expect(body.sessionId).toBe(computeSessionId(legionId, "eng-42", "implement"));
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
        promotedFile: `/tmp/legion-state/legions/${projectId}/promoted.json`,
        feedbackFile: `/tmp/legion-state/legions/${projectId}/feedback.jsonl`,
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
      symlink: async () => {},
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
    // For implement mode: blocking pre-dispatch fetch fires first, then clone + workspace add.
    expect(runJjCalls.slice(0, 3)).toEqual([
      ["git", "fetch", "-R", "/tmp/legion-data/repos/github.com/acme/widgets"],
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
        "--revision",
        "main",
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
          promotedFile: `/tmp/legion-state/legions/${projectId}/promoted.json`,
          feedbackFile: `/tmp/legion-state/legions/${projectId}/feedback.jsonl`,
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
        symlink: async () => {},
      };
      await startTestServer({ paths, repoManagerDeps });

      const response = await requestJson("/workers", {
        method: "POST",
        body: JSON.stringify({
          issueId: "acme-widgets-80",
          mode: "plan",
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
          mode: "plan",
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
      expect(await response.json()).toEqual({
        error: "missing_repo: provide --repo or ensure issue appears in collected state",
      });
    });
  });

  describe("POST /workers auto-resolve repo from cache", () => {
    const repoPaths: LegionPaths = {
      dataDir: "/tmp/legion-data",
      stateDir: "/tmp/legion-state",
      reposDir: "/tmp/legion-data/repos",
      workspacesDir: "/tmp/legion-data/workspaces",
      legionsFile: "/tmp/legion-state/legions.json",
      forLegion: (projectId: string) => ({
        legionStateDir: `/tmp/legion-state/legions/${projectId}`,
        workersFile: `/tmp/legion-state/legions/${projectId}/workers.json`,
        promotedFile: `/tmp/legion-state/legions/${projectId}/promoted.json`,
        feedbackFile: `/tmp/legion-state/legions/${projectId}/feedback.jsonl`,
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
      symlink: async () => {},
    };

    it("auto-resolves repo when cache has valid source", async () => {
      await startTestServer({ paths: repoPaths, repoManagerDeps });

      await requestJson("/state/collect", {
        method: "POST",
        body: JSON.stringify({
          backend: "github",
          issues: [
            {
              content: {
                number: 42,
                repository: "acme/widgets",
                url: "https://github.com/acme/widgets/issues/42",
                type: "Issue",
              },
              status: "Todo",
              labels: [],
            },
          ],
        }),
      });

      const response = await requestJson("/workers", {
        method: "POST",
        body: JSON.stringify({
          issueId: "acme-widgets-42",
          mode: "implement",
        }),
      });

      expect(response.status).toBe(200);
      expect(createSessionCalls).toHaveLength(1);
      expect(createSessionCalls[0].workspace).toBe(
        "/tmp/legion-data/workspaces/123e4567-e89b-12d3-a456-426614174000/acme-widgets-42"
      );
    });

    it("returns 400 when cache is empty", async () => {
      await startTestServer({ paths: repoPaths, repoManagerDeps });

      const response = await requestJson("/workers", {
        method: "POST",
        body: JSON.stringify({
          issueId: "acme-widgets-42",
          mode: "implement",
        }),
      });

      expect(response.status).toBe(400);
      const body = (await response.json()) as { error: string };
      expect(body.error).toContain("missing_repo");
    });

    it("returns 400 when cached issue has null source", async () => {
      await startTestServer({ paths: repoPaths, repoManagerDeps });

      await requestJson("/state/collect", {
        method: "POST",
        body: JSON.stringify({
          backend: "linear",
          issues: [
            {
              identifier: "LIN-99",
              state: { name: "In Progress" },
              labels: { nodes: [] },
            },
          ],
        }),
      });

      const response = await requestJson("/workers", {
        method: "POST",
        body: JSON.stringify({
          issueId: "LIN-99",
          mode: "implement",
        }),
      });

      expect(response.status).toBe(400);
      const body = (await response.json()) as { error: string };
      expect(body.error).toContain("missing_repo");
    });

    it("returns 400 when issue is not in cache", async () => {
      await startTestServer({ paths: repoPaths, repoManagerDeps });

      await requestJson("/state/collect", {
        method: "POST",
        body: JSON.stringify({
          backend: "github",
          issues: [
            {
              content: {
                number: 42,
                repository: "acme/widgets",
                url: "https://github.com/acme/widgets/issues/42",
                type: "Issue",
              },
              status: "Todo",
              labels: [],
            },
          ],
        }),
      });

      const response = await requestJson("/workers", {
        method: "POST",
        body: JSON.stringify({
          issueId: "acme-widgets-99",
          mode: "implement",
        }),
      });

      expect(response.status).toBe(400);
      const body = (await response.json()) as { error: string };
      expect(body.error).toContain("missing_repo");
    });
  });

  it("succeeds when clone exists even if background fetch would fail", async () => {
    const paths: LegionPaths = {
      dataDir: "/tmp/legion-data",
      stateDir: "/tmp/legion-state",
      reposDir: "/tmp/legion-data/repos",
      workspacesDir: "/tmp/legion-data/workspaces",
      legionsFile: "/tmp/legion-state/legions.json",
      forLegion: (projectId: string) => ({
        legionStateDir: `/tmp/legion-state/legions/${projectId}`,
        workersFile: `/tmp/legion-state/legions/${projectId}/workers.json`,
        promotedFile: `/tmp/legion-state/legions/${projectId}/promoted.json`,
        feedbackFile: `/tmp/legion-state/legions/${projectId}/feedback.jsonl`,
        logDir: `/tmp/legion-state/legions/${projectId}/logs`,
        workspacesDir: `/tmp/legion-data/workspaces/${projectId}`,
      }),
      repoClonePath: (host: string, owner: string, repo: string) =>
        `/tmp/legion-data/repos/${host}/${owner}/${repo}`,
    };
    const repoManagerDeps: RepoManagerDeps = {
      // runJj would fail if called — but ensureRepoClone skips fetch for existing clones
      runJj: async () => ({ exitCode: 128, stdout: "", stderr: "Permission denied (publickey)" }),
      exists: async () => true,
      rmDir: async () => {},
      symlink: async () => {},
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

    // Worker created successfully — background fetch failure is non-blocking
    expect(response.status).toBe(200);
    const body = await response.json();
    expect(body).toHaveProperty("id", "acme-widgets-99-implement");
    expect(body).toHaveProperty("sessionId");
  });

  it("runs blocking fetch before workspace creation for implement mode", async () => {
    const jjCommands: string[][] = [];
    const paths: LegionPaths = {
      dataDir: "/tmp/legion-data",
      stateDir: "/tmp/legion-state",
      reposDir: "/tmp/legion-data/repos",
      workspacesDir: "/tmp/legion-data/workspaces",
      legionsFile: "/tmp/legion-state/legions.json",
      forLegion: (projectId: string) => ({
        legionStateDir: `/tmp/legion-state/legions/${projectId}`,
        workersFile: `/tmp/legion-state/legions/${projectId}/workers.json`,
        promotedFile: `/tmp/legion-state/legions/${projectId}/promoted.json`,
        feedbackFile: `/tmp/legion-state/legions/${projectId}/feedback.jsonl`,
        logDir: `/tmp/legion-state/legions/${projectId}/logs`,
        workspacesDir: `/tmp/legion-data/workspaces/${projectId}`,
      }),
      repoClonePath: (host: string, owner: string, repo: string) =>
        `/tmp/legion-data/repos/${host}/${owner}/${repo}`,
    };
    const repoManagerDeps: RepoManagerDeps = {
      runJj: async (args) => {
        jjCommands.push([...args]);
        return { exitCode: 0, stdout: "", stderr: "" };
      },
      exists: async () => true,
      rmDir: async () => {},
      symlink: async () => {},
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

    expect(response.status).toBe(200);
    // For implement mode, the fetch should happen BEFORE workspace creation.
    // Since clone exists, ensureRepoClone doesn't call runJj, so the first
    // jj command should be "git fetch" (the blocking pre-dispatch fetch).
    const fetchCmd = jjCommands.find((c) => c[0] === "git" && c[1] === "fetch");
    expect(fetchCmd).toBeDefined();
    expect(fetchCmd).toContain("-R");
  });

  it("uses non-blocking fetch for non-implement modes", async () => {
    const jjCommands: string[][] = [];
    const paths: LegionPaths = {
      dataDir: "/tmp/legion-data",
      stateDir: "/tmp/legion-state",
      reposDir: "/tmp/legion-data/repos",
      workspacesDir: "/tmp/legion-data/workspaces",
      legionsFile: "/tmp/legion-state/legions.json",
      forLegion: (projectId: string) => ({
        legionStateDir: `/tmp/legion-state/legions/${projectId}`,
        workersFile: `/tmp/legion-state/legions/${projectId}/workers.json`,
        promotedFile: `/tmp/legion-state/legions/${projectId}/promoted.json`,
        feedbackFile: `/tmp/legion-state/legions/${projectId}/feedback.jsonl`,
        logDir: `/tmp/legion-state/legions/${projectId}/logs`,
        workspacesDir: `/tmp/legion-data/workspaces/${projectId}`,
      }),
      repoClonePath: (host: string, owner: string, repo: string) =>
        `/tmp/legion-data/repos/${host}/${owner}/${repo}`,
    };
    const repoManagerDeps: RepoManagerDeps = {
      runJj: async (args) => {
        jjCommands.push([...args]);
        return { exitCode: 0, stdout: "", stderr: "" };
      },
      exists: async () => true,
      rmDir: async () => {},
      symlink: async () => {},
    };
    await startTestServer({ paths, repoManagerDeps });

    const response = await requestJson("/workers", {
      method: "POST",
      body: JSON.stringify({
        issueId: "acme-widgets-99",
        mode: "plan",
        repo: "acme/widgets",
        force: true,
      }),
    });

    expect(response.status).toBe(200);
    // For plan mode, the fetch is non-blocking (background).
    // A fetch command should still eventually be issued, but we mainly verify
    // that the response succeeds and a fetch was queued.
    // Wait a tick for the background fetch to fire
    await new Promise((resolve) => setTimeout(resolve, 50));
    const fetchCmd = jjCommands.find((c) => c[0] === "git" && c[1] === "fetch");
    expect(fetchCmd).toBeDefined();
  });

  it("returns 500 when repo clone fails", async () => {
    const paths: LegionPaths = {
      dataDir: "/tmp/legion-data",
      stateDir: "/tmp/legion-state",
      reposDir: "/tmp/legion-data/repos",
      workspacesDir: "/tmp/legion-data/workspaces",
      legionsFile: "/tmp/legion-state/legions.json",
      forLegion: (projectId: string) => ({
        legionStateDir: `/tmp/legion-state/legions/${projectId}`,
        workersFile: `/tmp/legion-state/legions/${projectId}/workers.json`,
        promotedFile: `/tmp/legion-state/legions/${projectId}/promoted.json`,
        feedbackFile: `/tmp/legion-state/legions/${projectId}/feedback.jsonl`,
        logDir: `/tmp/legion-state/legions/${projectId}/logs`,
        workspacesDir: `/tmp/legion-data/workspaces/${projectId}`,
      }),
      repoClonePath: (host: string, owner: string, repo: string) =>
        `/tmp/legion-data/repos/${host}/${owner}/${repo}`,
    };
    const repoManagerDeps: RepoManagerDeps = {
      runJj: async () => ({ exitCode: 128, stdout: "", stderr: "Permission denied (publickey)" }),
      exists: async () => false, // clone does not exist — clone will fail
      rmDir: async () => {},
      symlink: async () => {},
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

  it("creates different session IDs for different versions", async () => {
    await startTestServer();

    const version1 = await requestJson("/workers", {
      method: "POST",
      body: JSON.stringify({ issueId: "ENG-88", mode: "implement", workspace: "/tmp", version: 1 }),
    });
    expect(version1.status).toBe(200);
    const version1Body = (await version1.json()) as { id: string; sessionId: string };

    await requestJson(`/workers/${version1Body.id}`, { method: "DELETE" });

    const version2 = await requestJson("/workers", {
      method: "POST",
      body: JSON.stringify({ issueId: "ENG-88", mode: "implement", workspace: "/tmp", version: 2 }),
    });
    expect(version2.status).toBe(200);
    const version2Body = (await version2.json()) as { sessionId: string };

    expect(version1Body.sessionId).not.toBe(version2Body.sessionId);
  });

  it("keeps session IDs deterministic for same version", async () => {
    await startTestServer();

    const first = await requestJson("/workers", {
      method: "POST",
      body: JSON.stringify({ issueId: "ENG-89", mode: "implement", workspace: "/tmp", version: 2 }),
    });
    expect(first.status).toBe(200);
    const firstBody = (await first.json()) as { id: string; sessionId: string };

    await requestJson(`/workers/${firstBody.id}`, { method: "DELETE" });

    const second = await requestJson("/workers", {
      method: "POST",
      body: JSON.stringify({ issueId: "ENG-89", mode: "implement", workspace: "/tmp", version: 2 }),
    });
    expect(second.status).toBe(200);
    const secondBody = (await second.json()) as { sessionId: string };

    expect(firstBody.sessionId).toBe(secondBody.sessionId);
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

    const sessionId = createSessionCalls[0].sessionId;
    const deleteResponse = await requestJson(`/workers/${created.id}`, { method: "DELETE" });
    expect(deleteResponse.status).toBe(200);
    expect(await deleteResponse.json()).toEqual({ status: "stopped" });

    // Session should be deleted from serve to release SQLite FDs
    expect(deleteSessionCalls).toEqual([sessionId]);

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
        promotedFile: `/tmp/legion-state/legions/${projectId}/promoted.json`,
        feedbackFile: `/tmp/legion-state/legions/${projectId}/feedback.jsonl`,
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
      symlink: async () => {},
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
    expect(await cleanupResponse.json()).toEqual({ status: "cleaned", workerRemoved: true });
    expect(runJjCalls).toHaveLength(2);
    // First call: verifyBranchPushed checks if bookmark is pushed
    expect(runJjCalls[0]).toContain("bookmark");
    expect(runJjCalls[0]).toContain("list");
    expect(runJjCalls[0]).toContain("eng-79");
    // Second call: workspace forget
    expect(runJjCalls[1]).toEqual([
      "workspace",
      "forget",
      "eng-79",
      "-R",
      "/tmp/legion-data/repos/github.com/acme/widgets",
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

  describe("state delta notifications", () => {
    it("does not publish on first state collection (baseline establishment)", async () => {
      await startTestServer({
        getControllerState: () => ({ sessionId: "ses_ctrl" }),
      });

      const response = await requestJson("/state/collect", {
        method: "POST",
        body: JSON.stringify({
          backend: "github",
          issues: [createGitHubProjectItem({ status: "Todo" })],
        }),
      });

      expect(response.status).toBe(200);
      await Bun.sleep(50);
      expect(mockEnvoy.publishCalls.length).toBe(0);
    });

    it("publishes delta on second collection with changes", async () => {
      await startTestServer({
        getControllerState: () => ({ sessionId: "ses_ctrl" }),
      });

      // Track the issue so changed entries are included in delta
      const trackResponse = await requestJson("/state/track", {
        method: "POST",
        body: JSON.stringify({ issueId: "acme-widgets-42" }),
      });
      expect(trackResponse.status).toBe(200);

      const firstResponse = await requestJson("/state/collect", {
        method: "POST",
        body: JSON.stringify({
          backend: "github",
          issues: [createGitHubProjectItem({ status: "Todo" })],
        }),
      });
      expect(firstResponse.status).toBe(200);
      await Bun.sleep(50);

      const secondResponse = await requestJson("/state/collect", {
        method: "POST",
        body: JSON.stringify({
          backend: "github",
          issues: [createGitHubProjectItem({ status: "In Progress" })],
        }),
      });
      expect(secondResponse.status).toBe(200);
      await Bun.sleep(50);

      expect(mockEnvoy.publishCalls.length).toBe(1);
      const payload = mockEnvoy.publishCalls[0] as { topic: string; message: string };
      expect(payload.topic).toBe("notifications.legion.controller");
      const delta = JSON.parse(payload.message) as {
        type: string;
        changes: {
          changed: Array<{ changedFields: string[] }>;
        };
      };
      expect(delta.type).toBe("state_delta");
      expect(delta.changes.changed.length).toBe(1);
      expect(delta.changes.changed[0]?.changedFields.includes("status")).toBe(true);
    });

    it("does not publish when state is identical (label order invariant)", async () => {
      await startTestServer({
        getControllerState: () => ({ sessionId: "ses_ctrl" }),
      });

      const firstResponse = await requestJson("/state/collect", {
        method: "POST",
        body: JSON.stringify({
          backend: "github",
          issues: [createGitHubProjectItem({ labels: ["worker-done", "test-passed"] })],
        }),
      });
      expect(firstResponse.status).toBe(200);
      await Bun.sleep(50);

      const secondResponse = await requestJson("/state/collect", {
        method: "POST",
        body: JSON.stringify({
          backend: "github",
          issues: [createGitHubProjectItem({ labels: ["test-passed", "worker-done"] })],
        }),
      });
      expect(secondResponse.status).toBe(200);
      await Bun.sleep(50);

      expect(mockEnvoy.publishCalls.length).toBe(0);
    });

    it("swallows publish errors without affecting collect response", async () => {
      const warnCalls: unknown[][] = [];
      // Override fetch to throw on publish — simulates network error, not server error.
      // This must use fetch interception because a mock HTTP server can't make fetch() reject.
      const publishAttempts: unknown[] = [];
      const mockFn = async (
        input: string | { href?: unknown; url?: unknown },
        init?: { body?: unknown }
      ) => {
        const url = getFetchUrl(input);
        if (url.includes("/v1/messages/publish")) {
          publishAttempts.push({ url, body: JSON.parse(init?.body as string) });
          throw new Error("publish boom");
        }
        return originalFetch(input as never, init as never);
      };
      globalThis.fetch = Object.assign(mockFn, {
        preconnect: originalFetch.preconnect,
      });
      console.warn = (...args: unknown[]) => {
        warnCalls.push(args);
      };

      await startTestServer({
        getControllerState: () => ({ sessionId: "ses_ctrl" }),
      });

      // Track the issue so changed entries are included in delta
      const trackResponse = await requestJson("/state/track", {
        method: "POST",
        body: JSON.stringify({ issueId: "acme-widgets-42" }),
      });
      expect(trackResponse.status).toBe(200);

      const firstResponse = await requestJson("/state/collect", {
        method: "POST",
        body: JSON.stringify({
          backend: "github",
          issues: [createGitHubProjectItem({ status: "Todo" })],
        }),
      });
      expect(firstResponse.status).toBe(200);
      await Bun.sleep(50);

      const secondResponse = await requestJson("/state/collect", {
        method: "POST",
        body: JSON.stringify({
          backend: "github",
          issues: [createGitHubProjectItem({ status: "In Progress" })],
        }),
      });
      expect(secondResponse.status).toBe(200);
      await Bun.sleep(50);

      expect(publishAttempts.length).toBe(1);
      expect(warnCalls.length).toBe(1);
      expect(
        String(warnCalls[0]?.[0] ?? "").includes("[state-delta] publish error (non-fatal)")
      ).toBe(true);
    });

    it("does not publish when no controller is active", async () => {
      await startTestServer({
        getControllerState: () => undefined,
      });

      const firstResponse = await requestJson("/state/collect", {
        method: "POST",
        body: JSON.stringify({
          backend: "github",
          issues: [createGitHubProjectItem({ status: "Todo" })],
        }),
      });
      expect(firstResponse.status).toBe(200);
      await Bun.sleep(50);

      const secondResponse = await requestJson("/state/collect", {
        method: "POST",
        body: JSON.stringify({
          backend: "github",
          issues: [createGitHubProjectItem({ status: "In Progress" })],
        }),
      });
      expect(secondResponse.status).toBe(200);
      await Bun.sleep(50);

      expect(mockEnvoy.publishCalls.length).toBe(0);
    });

    it("does not publish changed entries for untracked issues", async () => {
      await startTestServer({
        getControllerState: () => ({ sessionId: "ses_ctrl" }),
      });

      // Establish baseline — issue NOT tracked
      const firstResponse = await requestJson("/state/collect", {
        method: "POST",
        body: JSON.stringify({
          backend: "github",
          issues: [createGitHubProjectItem({ status: "Todo" })],
        }),
      });
      expect(firstResponse.status).toBe(200);
      await Bun.sleep(50);

      // Issue changes status — but is not tracked, so no changed entry should be published
      const secondResponse = await requestJson("/state/collect", {
        method: "POST",
        body: JSON.stringify({
          backend: "github",
          issues: [createGitHubProjectItem({ status: "In Progress" })],
        }),
      });
      expect(secondResponse.status).toBe(200);
      await Bun.sleep(50);

      // No publish because changed entries are filtered to tracked set
      expect(mockEnvoy.publishCalls.length).toBe(0);
    });

    it("publishes changed entries only for tracked issues when mixed", async () => {
      await startTestServer({
        getControllerState: () => ({ sessionId: "ses_ctrl" }),
      });

      // Track issue #42 but not #99
      await requestJson("/state/track", {
        method: "POST",
        body: JSON.stringify({ issueId: "acme-widgets-42" }),
      });

      // Establish baseline with both issues
      await requestJson("/state/collect", {
        method: "POST",
        body: JSON.stringify({
          backend: "github",
          issues: [
            createGitHubProjectItem({ number: 42, status: "Todo" }),
            createGitHubProjectItem({ number: 99, status: "Todo" }),
          ],
        }),
      });
      await Bun.sleep(50);

      // Both issues change — only tracked #42 should appear in delta
      const secondResponse = await requestJson("/state/collect", {
        method: "POST",
        body: JSON.stringify({
          backend: "github",
          issues: [
            createGitHubProjectItem({ number: 42, status: "In Progress" }),
            createGitHubProjectItem({ number: 99, status: "In Progress" }),
          ],
        }),
      });
      expect(secondResponse.status).toBe(200);
      await Bun.sleep(50);

      expect(mockEnvoy.publishCalls.length).toBe(1);
      const delta = JSON.parse(
        (mockEnvoy.publishCalls[0] as { topic: string; message: string }).message
      ) as { changes: { changed: Array<{ issueId: string }> } };
      expect(delta.changes.changed).toHaveLength(1);
      expect(delta.changes.changed[0]?.issueId).toBe("acme-widgets-42");
    });

    it("health tick interleaving does not produce false deltas", async () => {
      await startTestServer({
        legionId: "acme/123",
        getControllerState: () => ({ sessionId: "ses_ctrl" }),
        fetchProjectItems: async () => [createGitHubProjectItem({ status: "Todo" })],
      });

      // Establish baseline via controller-initiated /state/collect
      const firstResponse = await requestJson("/state/collect", {
        method: "POST",
        body: JSON.stringify({
          backend: "github",
          issues: [createGitHubProjectItem({ status: "Todo" })],
        }),
      });
      expect(firstResponse.status).toBe(200);
      await Bun.sleep(50);

      // Run health tick (fetchAndProcessState) — should NOT affect baseline
      expect(serverFetchAndProcessState).not.toBeNull();
      await serverFetchAndProcessState?.();
      await Bun.sleep(50);

      // Re-collect identical data via controller — should produce zero deltas
      const secondResponse = await requestJson("/state/collect", {
        method: "POST",
        body: JSON.stringify({
          backend: "github",
          issues: [createGitHubProjectItem({ status: "Todo" })],
        }),
      });
      expect(secondResponse.status).toBe(200);
      await Bun.sleep(50);

      expect(mockEnvoy.publishCalls.length).toBe(0);
    });

    it("health tick with different issue set does not produce false deltas", async () => {
      await startTestServer({
        legionId: "acme/123",
        getControllerState: () => ({ sessionId: "ses_ctrl" }),
        fetchProjectItems: async () => [
          createGitHubProjectItem({ number: 99, status: "In Progress" }),
        ],
      });

      // Establish baseline via controller with issue 42
      const firstResponse = await requestJson("/state/collect", {
        method: "POST",
        body: JSON.stringify({
          backend: "github",
          issues: [createGitHubProjectItem({ status: "Todo" })],
        }),
      });
      expect(firstResponse.status).toBe(200);
      await Bun.sleep(50);

      // Health tick fetches different issue set (number: 99) — should NOT corrupt baseline
      expect(serverFetchAndProcessState).not.toBeNull();
      await serverFetchAndProcessState?.();
      await Bun.sleep(50);

      // Re-collect same original data — should produce zero deltas
      const secondResponse = await requestJson("/state/collect", {
        method: "POST",
        body: JSON.stringify({
          backend: "github",
          issues: [createGitHubProjectItem({ status: "Todo" })],
        }),
      });
      expect(secondResponse.status).toBe(200);
      await Bun.sleep(50);

      expect(mockEnvoy.publishCalls.length).toBe(0);
    });

    it("health tick still populates issueStateCache for dispatch", async () => {
      await startTestServer({
        legionId: "acme/123",
        fetchProjectItems: async () => [createGitHubProjectItem({ status: "Todo" })],
      });

      // Run health tick to populate cache
      expect(serverFetchAndProcessState).not.toBeNull();
      await serverFetchAndProcessState?.();
      await Bun.sleep(50);

      // Dispatch without repo — should succeed via cache auto-resolution
      const response = await requestJson("/workers", {
        method: "POST",
        body: JSON.stringify({
          issueId: "acme-widgets-42",
          mode: "implement",
        }),
      });

      // Should NOT fail with missing_repo — cache provides the repo
      const body = (await response.json()) as { error?: string; id?: string };
      expect(body.error).not.toBe("missing_repo");
    });

    it("fetch-and-collect still publishes delta on changed second collection", async () => {
      let fetchStatus = "Todo";
      await startTestServer({
        legionId: "acme/123",
        getControllerState: () => ({ sessionId: "ses_ctrl" }),
        fetchProjectItems: async () => [createGitHubProjectItem({ status: fetchStatus })],
      });

      // Track the issue so changed entries are included in delta
      const trackResponse = await requestJson("/state/track", {
        method: "POST",
        body: JSON.stringify({ issueId: "acme-widgets-42" }),
      });
      expect(trackResponse.status).toBe(200);

      // First fetch-and-collect establishes baseline
      const firstResponse = await requestJson("/state/fetch-and-collect", {
        method: "POST",
        body: JSON.stringify({ backend: "github" }),
      });
      expect(firstResponse.status).toBe(200);
      await Bun.sleep(50);
      expect(mockEnvoy.publishCalls.length).toBe(0);

      // Change fetcher response and fetch again — should publish delta
      fetchStatus = "In Progress";
      const secondResponse = await requestJson("/state/fetch-and-collect", {
        method: "POST",
        body: JSON.stringify({ backend: "github" }),
      });
      expect(secondResponse.status).toBe(200);
      await Bun.sleep(50);

      expect(mockEnvoy.publishCalls.length).toBe(1);
    });
  });

  describe("auto-cleanup on state collect", () => {
    const paths: LegionPaths = {
      dataDir: "/tmp/legion-data",
      stateDir: "/tmp/legion-state",
      reposDir: "/tmp/legion-data/repos",
      workspacesDir: "/tmp/legion-data/workspaces",
      legionsFile: "/tmp/legion-state/legions.json",
      forLegion: (projectId: string) => ({
        legionStateDir: `/tmp/legion-state/legions/${projectId}`,
        workersFile: `/tmp/legion-state/legions/${projectId}/workers.json`,
        promotedFile: `/tmp/legion-state/legions/${projectId}/promoted.json`,
        feedbackFile: `/tmp/legion-state/legions/${projectId}/feedback.jsonl`,
        logDir: `/tmp/legion-state/legions/${projectId}/logs`,
        workspacesDir: `/tmp/legion-data/workspaces/${projectId}`,
      }),
      repoClonePath: (host: string, owner: string, repo: string) =>
        `/tmp/legion-data/repos/${host}/${owner}/${repo}`,
    };

    function makeRepoManagerDeps(overrides?: Partial<RepoManagerDeps>): RepoManagerDeps {
      return {
        runJj: async () => ({ exitCode: 0, stdout: "", stderr: "" }),
        exists: async () => true,
        rmDir: async () => {},
        symlink: async () => {},
        ...overrides,
      };
    }

    async function createRepoWorker(issueId: string) {
      const response = await requestJson("/workers", {
        method: "POST",
        body: JSON.stringify({
          issueId,
          mode: "implement",
          repo: "acme/widgets",
        }),
      });
      expect(response.status).toBe(200);
      return (await response.json()) as { id: string; sessionId: string };
    }

    it("auto-cleans workers for Done issues on state collect", async () => {
      const rmDirCalls: string[] = [];
      await startTestServer({
        paths,
        repoManagerDeps: makeRepoManagerDeps({
          rmDir: async (workspacePath: string) => {
            rmDirCalls.push(workspacePath);
          },
        }),
      });

      const created = await createRepoWorker("acme-widgets-42");

      const response = await requestJson("/state/collect", {
        method: "POST",
        body: JSON.stringify({
          backend: "github",
          issues: [
            {
              id: "PVTI_abc",
              content: {
                number: 42,
                repository: "acme/widgets",
                url: "https://github.com/acme/widgets/issues/42",
                type: "Issue",
              },
              status: "Done",
              labels: [],
            },
          ],
        }),
      });

      expect(response.status).toBe(200);
      await new Promise((resolve) => setTimeout(resolve, 50));

      const workersResponse = await requestJson("/workers");
      expect((await workersResponse.json()) as WorkerEntry[]).toEqual([]);
      expect(deleteSessionCalls).toEqual([created.sessionId]);
      expect(rmDirCalls).toEqual([
        "/tmp/legion-data/workspaces/123e4567-e89b-12d3-a456-426614174000/acme-widgets-42",
      ]);
    });

    it("does not clean workers for non-Done issues", async () => {
      await startTestServer({
        paths,
        repoManagerDeps: makeRepoManagerDeps(),
      });

      await createRepoWorker("acme-widgets-43");

      const response = await requestJson("/state/collect", {
        method: "POST",
        body: JSON.stringify({
          backend: "github",
          issues: [
            {
              id: "PVTI_def",
              content: {
                number: 43,
                repository: "acme/widgets",
                url: "https://github.com/acme/widgets/issues/43",
                type: "Issue",
              },
              status: "In Progress",
              labels: [],
            },
          ],
        }),
      });

      expect(response.status).toBe(200);
      await new Promise((resolve) => setTimeout(resolve, 50));

      const workersResponse = await requestJson("/workers");
      const workers = (await workersResponse.json()) as WorkerEntry[];
      expect(workers).toHaveLength(1);
      expect(workers[0]?.id).toBe("acme-widgets-43-implement");
      expect(deleteSessionCalls).toEqual([]);
    });

    it("auto-cleanup is idempotent", async () => {
      const rmDirCalls: string[] = [];
      await startTestServer({
        paths,
        repoManagerDeps: makeRepoManagerDeps({
          rmDir: async (workspacePath: string) => {
            rmDirCalls.push(workspacePath);
          },
        }),
      });

      const created = await createRepoWorker("acme-widgets-44");

      const donePayload = JSON.stringify({
        backend: "github",
        issues: [
          {
            id: "PVTI_xyz",
            content: {
              number: 44,
              repository: "acme/widgets",
              url: "https://github.com/acme/widgets/issues/44",
              type: "Issue",
            },
            status: "Done",
            labels: [],
          },
        ],
      });

      const first = await requestJson("/state/collect", {
        method: "POST",
        body: donePayload,
      });
      expect(first.status).toBe(200);
      await new Promise((resolve) => setTimeout(resolve, 50));

      const second = await requestJson("/state/collect", {
        method: "POST",
        body: donePayload,
      });
      expect(second.status).toBe(200);
      await new Promise((resolve) => setTimeout(resolve, 50));

      const workersResponse = await requestJson("/workers");
      expect((await workersResponse.json()) as WorkerEntry[]).toEqual([]);
      expect(deleteSessionCalls).toEqual([created.sessionId]);
      expect(rmDirCalls).toEqual([
        "/tmp/legion-data/workspaces/123e4567-e89b-12d3-a456-426614174000/acme-widgets-44",
      ]);
    });

    it("auto-cleans workers on fetch-and-collect", async () => {
      const rmDirCalls: string[] = [];
      await startTestServer({
        paths,
        repoManagerDeps: makeRepoManagerDeps({
          rmDir: async (workspacePath: string) => {
            rmDirCalls.push(workspacePath);
          },
        }),
      });

      const created = await createRepoWorker("acme-widgets-45");
      Bun.spawn = ((cmd: string[]) => {
        expect(cmd.slice(0, 3)).toEqual(["gh", "api", "graphql"]);
        return {
          stdout: new Blob([
            JSON.stringify({
              data: {
                organization: {
                  projectV2: {
                    items: {
                      pageInfo: { hasNextPage: false, endCursor: null },
                      nodes: [
                        {
                          id: "PVTI_fetch",
                          fieldValueByName: { name: "Done" },
                          labels: { nodes: [] },
                          content: {
                            __typename: "Issue",
                            number: 45,
                            title: "Done issue",
                            url: "https://github.com/acme/widgets/issues/45",
                            repository: { nameWithOwner: "acme/widgets" },
                            issueDependenciesSummary: { blockedBy: 0 },
                            linkedPullRequests: { nodes: [] },
                          },
                        },
                      ],
                    },
                  },
                },
              },
            }),
          ]).stream(),
          stderr: new Blob([""]).stream(),
          exited: Promise.resolve(0),
          kill: () => {},
        } as unknown as ReturnType<typeof Bun.spawn>;
      }) as typeof Bun.spawn;

      const response = await requestJson("/state/fetch-and-collect", {
        method: "POST",
        body: JSON.stringify({ backend: "github", legionId: "acme/123" }),
      });

      expect(response.status).toBe(200);
      await new Promise((resolve) => setTimeout(resolve, 50));

      const workersResponse = await requestJson("/workers");
      expect((await workersResponse.json()) as WorkerEntry[]).toEqual([]);
      expect(deleteSessionCalls).toEqual([created.sessionId]);
      expect(rmDirCalls).toEqual([
        "/tmp/legion-data/workspaces/123e4567-e89b-12d3-a456-426614174000/acme-widgets-45",
      ]);
    });

    it("preserves worker state when workspace cleanup fails", async () => {
      await startTestServer({
        paths,
        repoManagerDeps: makeRepoManagerDeps({
          rmDir: async () => {
            throw new Error("disk I/O error");
          },
        }),
      });

      await createRepoWorker("acme-widgets-46");

      const response = await requestJson("/state/collect", {
        method: "POST",
        body: JSON.stringify({
          backend: "github",
          issues: [
            {
              id: "PVTI_fail",
              content: {
                number: 46,
                repository: "acme/widgets",
                url: "https://github.com/acme/widgets/issues/46",
                type: "Issue",
              },
              status: "Done",
              labels: [],
            },
          ],
        }),
      });

      expect(response.status).toBe(200);
      await new Promise((resolve) => setTimeout(resolve, 50));

      // Worker should still exist because workspace cleanup failed
      const workersResponse = await requestJson("/workers");
      const workers = (await workersResponse.json()) as WorkerEntry[];
      expect(workers).toHaveLength(1);
      expect(workers[0]?.id).toBe("acme-widgets-46-implement");
      // Session should NOT have been deleted
      expect(deleteSessionCalls).toEqual([]);
    });

    it("fires background fetch on repo clone after Done issue cleanup", async () => {
      const jjCommands: string[][] = [];
      const rmDirCalls: string[] = [];
      await startTestServer({
        paths,
        repoManagerDeps: makeRepoManagerDeps({
          runJj: async (args) => {
            jjCommands.push([...args]);
            // bookmark list returns synced bookmark for cleanup to proceed
            if (args.includes("bookmark")) {
              return {
                exitCode: 0,
                stdout: ["acme-widgets-47 local", "acme-widgets-47 remote:origin ahead:0"].join(
                  "\n"
                ),
                stderr: "",
              };
            }
            return { exitCode: 0, stdout: "", stderr: "" };
          },
          rmDir: async (p) => {
            rmDirCalls.push(p);
          },
        }),
      });

      await createRepoWorker("acme-widgets-47");

      // Collect state with issue as Done — triggers cleanup + fetch
      await requestJson("/state/collect", {
        method: "POST",
        body: JSON.stringify({
          backend: "github",
          issues: [
            {
              id: "PVTI_fetch_on_close",
              content: {
                number: 47,
                repository: "acme/widgets",
                url: "https://github.com/acme/widgets/issues/47",
                type: "Issue",
              },
              status: "Done",
              labels: [],
            },
          ],
        }),
      });
      // Wait for async cleanup + background fetch to complete
      await new Promise((resolve) => setTimeout(resolve, 100));

      // Verify workspace was cleaned up
      expect(rmDirCalls.length).toBeGreaterThan(0);
      // Verify a "git fetch" was fired on the repo clone after cleanup
      // (separate from the pre-dispatch fetch for the implement worker creation)
      const fetchCommands = jjCommands.filter((c) => c[0] === "git" && c[1] === "fetch");
      // At least 2 fetches: one for implement dispatch (blocking), one after Done cleanup
      expect(fetchCommands.length).toBeGreaterThanOrEqual(2);
    });

    it("directory scan uses real fs listDir when repoManagerDeps.listDir is not injected", async () => {
      // Simulate production: repoManagerDeps has rmDir captured but no listDir,
      // so opts.repoManagerDeps?.listDir is undefined and the fallback fires.
      const rmDirCalls: string[] = [];
      const scanWorkspacesDir = await mkdtemp(path.join(os.tmpdir(), "legion-scan-"));
      try {
        // Create two workspace dirs on disk: one off-board, one on-board
        await mkdir(path.join(scanWorkspacesDir, "acme-widgets-51"));
        await mkdir(path.join(scanWorkspacesDir, "acme-widgets-999"));

        const scanPaths: LegionPaths = {
          ...paths,
          forLegion: (projectId: string) => ({
            ...paths.forLegion(projectId),
            workspacesDir: scanWorkspacesDir,
          }),
        };

        // No listDir in repoManagerDeps — fallback to defaultDeps.listDir must fire
        await startTestServer({
          paths: scanPaths,
          repoManagerDeps: makeRepoManagerDeps({
            rmDir: async (p: string) => {
              rmDirCalls.push(p);
            },
          }),
        });

        // acme-widgets-51 is on the board; acme-widgets-999 is off-board
        const response = await requestJson("/state/collect", {
          method: "POST",
          body: JSON.stringify({
            backend: "github",
            issues: [
              {
                id: "PVTI_scan1",
                content: {
                  number: 51,
                  repository: "acme/widgets",
                  url: "https://github.com/acme/widgets/issues/51",
                  type: "Issue",
                },
                status: "In Progress",
                labels: [],
              },
            ],
          }),
        });

        expect(response.status).toBe(200);
        await new Promise((resolve) => setTimeout(resolve, 50));

        // Only the off-board workspace should be removed
        expect(rmDirCalls).toHaveLength(1);
        expect(rmDirCalls[0]).toBe(path.join(scanWorkspacesDir, "acme-widgets-999"));
      } finally {
        await rm(scanWorkspacesDir, { recursive: true, force: true });
      }
    });

    it("directory scan skips workspace for issue with active worker when listDir not injected", async () => {
      const rmDirCalls: string[] = [];
      const scanWorkspacesDir = await mkdtemp(path.join(os.tmpdir(), "legion-scan-"));
      try {
        await mkdir(path.join(scanWorkspacesDir, "acme-widgets-52"));

        const scanPaths: LegionPaths = {
          ...paths,
          forLegion: (projectId: string) => ({
            ...paths.forLegion(projectId),
            workspacesDir: scanWorkspacesDir,
          }),
        };

        await startTestServer({
          paths: scanPaths,
          repoManagerDeps: makeRepoManagerDeps({
            rmDir: async (p: string) => {
              rmDirCalls.push(p);
            },
          }),
        });

        // Dispatch a worker for acme-widgets-52 so it has an active worker entry
        await createRepoWorker("acme-widgets-52");

        // acme-widgets-52 is NOT on the board but has an active worker — must not be removed
        const response = await requestJson("/state/collect", {
          method: "POST",
          body: JSON.stringify({ backend: "github", issues: [] }),
        });

        expect(response.status).toBe(200);
        await new Promise((resolve) => setTimeout(resolve, 50));

        expect(rmDirCalls).toHaveLength(0);
      } finally {
        await rm(scanWorkspacesDir, { recursive: true, force: true });
      }
    });
  });

  describe("dead worker cleanup", () => {
    const paths: LegionPaths = {
      dataDir: "/tmp/legion-data",
      stateDir: "/tmp/legion-state",
      reposDir: "/tmp/legion-data/repos",
      workspacesDir: "/tmp/legion-data/workspaces",
      legionsFile: "/tmp/legion-state/legions.json",
      forLegion: (projectId: string) => ({
        legionStateDir: `/tmp/legion-state/legions/${projectId}`,
        workersFile: `/tmp/legion-state/legions/${projectId}/workers.json`,
        promotedFile: `/tmp/legion-state/legions/${projectId}/promoted.json`,
        feedbackFile: `/tmp/legion-state/legions/${projectId}/feedback.jsonl`,
        logDir: `/tmp/legion-state/legions/${projectId}/logs`,
        workspacesDir: `/tmp/legion-data/workspaces/${projectId}`,
      }),
      repoClonePath: (host: string, owner: string, repo: string) =>
        `/tmp/legion-data/repos/${host}/${owner}/${repo}`,
    };

    function makeRepoManagerDeps(overrides?: Partial<RepoManagerDeps>): RepoManagerDeps {
      return {
        runJj: async () => ({ exitCode: 0, stdout: "", stderr: "" }),
        exists: async () => true,
        rmDir: async () => {},
        symlink: async () => {},
        ...overrides,
      };
    }

    async function createRepoWorker(issueId: string) {
      const response = await requestJson("/workers", {
        method: "POST",
        body: JSON.stringify({
          issueId,
          mode: "implement",
          repo: "acme/widgets",
        }),
      });
      expect(response.status).toBe(200);
      return (await response.json()) as { id: string; sessionId: string };
    }

    it("removes dead worker state without touching workspaces", async () => {
      const rmDirCalls: string[] = [];
      const { server, stop, cleanupDeadWorkers } = startServer({
        port: 0,
        hostname: "127.0.0.1",
        envoyUrl: mockEnvoy.url,
        legionId,
        paths,
        adapter: makeAdapter(),
        repoManagerDeps: makeRepoManagerDeps({
          rmDir: async (workspacePath: string) => {
            rmDirCalls.push(workspacePath);
          },
        }),
        stateFilePath: path.join(
          await mkdtemp(path.join(os.tmpdir(), "legion-dead-")),
          "workers.json"
        ),
      });
      const localBaseUrl = `http://127.0.0.1:${server.port}`;
      try {
        // Create a worker, then mark it dead
        const createRes = await originalFetch(`${localBaseUrl}/workers`, {
          method: "POST",
          headers: { "content-type": "application/json" },
          body: JSON.stringify({
            issueId: "acme-widgets-60",
            mode: "implement",
            repo: "acme/widgets",
          }),
        });
        expect(createRes.status).toBe(200);
        const created = (await createRes.json()) as { id: string; sessionId: string };

        const patchRes = await originalFetch(`${localBaseUrl}/workers/${created.id}`, {
          method: "PATCH",
          headers: { "content-type": "application/json" },
          body: JSON.stringify({ status: "dead" }),
        });
        expect(patchRes.status).toBe(200);

        // Run cleanup
        await cleanupDeadWorkers();

        // Workspace should NOT be deleted (dead workers keep workspaces)
        expect(rmDirCalls).toHaveLength(0);

        // But worker should be removed from the map
        const workersRes = await originalFetch(`${localBaseUrl}/workers`);
        const workers = (await workersRes.json()) as WorkerEntry[];
        expect(workers).toEqual([]);
      } finally {
        stop();
      }
    });

    it("cleanupDeadWorkers does NOT delete workspaces (state cleanup only)", async () => {
      const rmDirCalls: string[] = [];
      const jjCalls: string[][] = [];
      const { server, stop, cleanupDeadWorkers } = startServer({
        port: 0,
        hostname: "127.0.0.1",
        envoyUrl: mockEnvoy.url,
        legionId,
        paths,
        adapter: makeAdapter(),
        repoManagerDeps: makeRepoManagerDeps({
          runJj: async (args) => {
            jjCalls.push(args);
            return { exitCode: 0, stdout: "", stderr: "" };
          },
          rmDir: async (workspacePath: string) => {
            rmDirCalls.push(workspacePath);
          },
        }),
        stateFilePath: path.join(
          await mkdtemp(path.join(os.tmpdir(), "legion-dead-")),
          "workers.json"
        ),
      });
      const localBaseUrl = `http://127.0.0.1:${server.port}`;
      try {
        // Create a worker, then mark it dead
        const createRes = await originalFetch(`${localBaseUrl}/workers`, {
          method: "POST",
          headers: { "content-type": "application/json" },
          body: JSON.stringify({
            issueId: "acme-widgets-99",
            mode: "implement",
            repo: "acme/widgets",
          }),
        });
        expect(createRes.status).toBe(200);
        const created = (await createRes.json()) as { id: string; sessionId: string };

        const patchRes = await originalFetch(`${localBaseUrl}/workers/${created.id}`, {
          method: "PATCH",
          headers: { "content-type": "application/json" },
          body: JSON.stringify({ status: "dead" }),
        });
        expect(patchRes.status).toBe(200);

        // Run cleanup
        await cleanupDeadWorkers();

        // Workspace should NOT be deleted — dead workers keep their workspaces
        expect(rmDirCalls).toHaveLength(0);
        const forgetCalls = jjCalls.filter(
          (args) => args[0] === "workspace" && args[1] === "forget"
        );
        expect(forgetCalls).toHaveLength(0);

        // But worker should still be removed from the map (state cleaned)
        const workersRes = await originalFetch(`${localBaseUrl}/workers`);
        const workers = (await workersRes.json()) as WorkerEntry[];
        expect(workers).toEqual([]);
      } finally {
        stop();
      }
    });

    it("does not clean up running workers", async () => {
      const rmDirCalls: string[] = [];
      await startTestServer({
        paths,
        repoManagerDeps: makeRepoManagerDeps({
          rmDir: async (workspacePath: string) => {
            rmDirCalls.push(workspacePath);
          },
        }),
      });

      // Create a worker but leave it in "starting" status (default)
      await createRepoWorker("acme-widgets-61");

      // Run cleanup via fetchAndProcessState (which triggers cleanup path)
      // Dead worker cleanup only targets status === "dead"
      const workersRes = await requestJson("/workers");
      const workers = (await workersRes.json()) as WorkerEntry[];
      expect(workers).toHaveLength(1);
      expect(rmDirCalls).toEqual([]);
    });

    it("still removes dead worker state even when jj is broken (no workspace cleanup)", async () => {
      const { server, stop, cleanupDeadWorkers } = startServer({
        port: 0,
        hostname: "127.0.0.1",
        envoyUrl: mockEnvoy.url,
        legionId,
        paths,
        adapter: makeAdapter(),
        repoManagerDeps: makeRepoManagerDeps({
          runJj: async () => {
            throw new Error("jj workspace forget failed");
          },
        }),
        stateFilePath: path.join(
          await mkdtemp(path.join(os.tmpdir(), "legion-dead-")),
          "workers.json"
        ),
      });
      const localBaseUrl = `http://127.0.0.1:${server.port}`;
      try {
        // Create and mark dead
        const createRes = await originalFetch(`${localBaseUrl}/workers`, {
          method: "POST",
          headers: { "content-type": "application/json" },
          body: JSON.stringify({
            issueId: "acme-widgets-62",
            mode: "implement",
            repo: "acme/widgets",
          }),
        });
        expect(createRes.status).toBe(200);
        const created = (await createRes.json()) as { id: string };

        await originalFetch(`${localBaseUrl}/workers/${created.id}`, {
          method: "PATCH",
          headers: { "content-type": "application/json" },
          body: JSON.stringify({ status: "dead" }),
        });

        // Run cleanup — no workspace cleanup happens (workers keep workspaces on death),
        // so the failing jj mock is irrelevant. Worker should still be removed.
        await cleanupDeadWorkers();

        // Worker removed (no workspace cleanup to fail)
        const workersRes = await originalFetch(`${localBaseUrl}/workers`);
        const workers = (await workersRes.json()) as WorkerEntry[];
        expect(workers).toHaveLength(0);
      } finally {
        stop();
      }
    });

    it("is idempotent — second call is a no-op after first succeeds", async () => {
      const rmDirCalls: string[] = [];
      const { server, stop, cleanupDeadWorkers } = startServer({
        port: 0,
        hostname: "127.0.0.1",
        envoyUrl: mockEnvoy.url,
        legionId,
        paths,
        adapter: makeAdapter(),
        repoManagerDeps: makeRepoManagerDeps({
          rmDir: async (workspacePath: string) => {
            rmDirCalls.push(workspacePath);
          },
        }),
        stateFilePath: path.join(
          await mkdtemp(path.join(os.tmpdir(), "legion-dead-")),
          "workers.json"
        ),
      });
      const localBaseUrl = `http://127.0.0.1:${server.port}`;
      try {
        const createRes = await originalFetch(`${localBaseUrl}/workers`, {
          method: "POST",
          headers: { "content-type": "application/json" },
          body: JSON.stringify({
            issueId: "acme-widgets-63",
            mode: "implement",
            repo: "acme/widgets",
          }),
        });
        expect(createRes.status).toBe(200);
        const created = (await createRes.json()) as { id: string };

        await originalFetch(`${localBaseUrl}/workers/${created.id}`, {
          method: "PATCH",
          headers: { "content-type": "application/json" },
          body: JSON.stringify({ status: "dead" }),
        });

        // First cleanup removes the worker (no workspace deletion)
        await cleanupDeadWorkers();

        // Second cleanup is a no-op
        await cleanupDeadWorkers();

        // No workspace deletion happened
        expect(rmDirCalls).toHaveLength(0);
      } finally {
        stop();
      }
    });

    it("cleans up multiple dead workers for the same issue", async () => {
      const rmDirCalls: string[] = [];
      const { server, stop, cleanupDeadWorkers } = startServer({
        port: 0,
        hostname: "127.0.0.1",
        envoyUrl: mockEnvoy.url,
        legionId,
        paths,
        adapter: makeAdapter(),
        repoManagerDeps: makeRepoManagerDeps({
          rmDir: async (workspacePath: string) => {
            rmDirCalls.push(workspacePath);
          },
        }),
        stateFilePath: path.join(
          await mkdtemp(path.join(os.tmpdir(), "legion-dead-")),
          "workers.json"
        ),
      });
      const localBaseUrl = `http://127.0.0.1:${server.port}`;
      try {
        // Create two workers for the same issue (different modes)
        for (const mode of ["implement", "test"]) {
          const createRes = await originalFetch(`${localBaseUrl}/workers`, {
            method: "POST",
            headers: { "content-type": "application/json" },
            body: JSON.stringify({
              issueId: "acme-widgets-64",
              mode,
              repo: "acme/widgets",
            }),
          });
          expect(createRes.status).toBe(200);
          const created = (await createRes.json()) as { id: string };

          await originalFetch(`${localBaseUrl}/workers/${created.id}`, {
            method: "PATCH",
            headers: { "content-type": "application/json" },
            body: JSON.stringify({ status: "dead" }),
          });
        }

        await cleanupDeadWorkers();

        // No workspace deletion (dead workers keep workspaces)
        expect(rmDirCalls).toHaveLength(0);

        // Both workers should be removed
        const workersRes = await originalFetch(`${localBaseUrl}/workers`);
        const workers = (await workersRes.json()) as WorkerEntry[];
        expect(workers).toEqual([]);
      } finally {
        stop();
      }
    });

    it("allows new dispatch after dead worker is cleaned up", async () => {
      const { server, stop, cleanupDeadWorkers } = startServer({
        port: 0,
        hostname: "127.0.0.1",
        envoyUrl: mockEnvoy.url,
        legionId,
        paths,
        adapter: makeAdapter(),
        repoManagerDeps: makeRepoManagerDeps(),
        stateFilePath: path.join(
          await mkdtemp(path.join(os.tmpdir(), "legion-dead-")),
          "workers.json"
        ),
      });
      const localBaseUrl = `http://127.0.0.1:${server.port}`;
      try {
        // Create, mark dead, cleanup
        const createRes = await originalFetch(`${localBaseUrl}/workers`, {
          method: "POST",
          headers: { "content-type": "application/json" },
          body: JSON.stringify({
            issueId: "acme-widgets-65",
            mode: "implement",
            repo: "acme/widgets",
          }),
        });
        expect(createRes.status).toBe(200);
        const created = (await createRes.json()) as { id: string };

        await originalFetch(`${localBaseUrl}/workers/${created.id}`, {
          method: "PATCH",
          headers: { "content-type": "application/json" },
          body: JSON.stringify({ status: "dead" }),
        });

        await cleanupDeadWorkers();

        // Should be able to create a new worker for the same issue
        const newRes = await originalFetch(`${localBaseUrl}/workers`, {
          method: "POST",
          headers: { "content-type": "application/json" },
          body: JSON.stringify({
            issueId: "acme-widgets-65",
            mode: "implement",
            repo: "acme/widgets",
          }),
        });
        expect(newRes.status).toBe(200);
        const newWorker = (await newRes.json()) as { id: string };
        expect(newWorker.id).toBe("acme-widgets-65-implement");
      } finally {
        stop();
      }
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
      envoyUrl: "",
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

  it("restarts on request (keeps serve alive)", async () => {
    let restartCalls = 0;
    await startTestServer();
    stopServer?.();
    stopServer = null;

    const { server, stop } = startServer({
      port: 0,
      hostname: "127.0.0.1",
      envoyUrl: "",
      legionId,
      legionDir: tempDir ?? os.tmpdir(),
      adapter: makeAdapter(),
      stateFilePath: path.join(tempDir ?? os.tmpdir(), "workers.json"),
      restartFn: async () => {
        restartCalls += 1;
      },
    });
    stopServer = stop;
    baseUrl = `http://127.0.0.1:${server.port}`;

    const response = await requestJson("/restart", { method: "POST" });
    expect(response.status).toBe(200);
    expect(await response.json()).toEqual({ status: "restarting" });
    expect(restartCalls).toBe(1);
  });

  it("returns 500 when restart is not supported", async () => {
    await startTestServer();
    // Default test server has no restartFn
    const response = await requestJson("/restart", { method: "POST" });
    expect(response.status).toBe(500);
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

    describe("multi-board fetch-and-collect", () => {
      let boardMocks = new Map<string, unknown>();

      function makeFetchProjectItems() {
        return async (owner: string, projectNumber: number) => {
          const key = `${owner}/${projectNumber}`;
          const result = boardMocks.get(key);
          if (result instanceof Error) {
            throw result;
          }
          if (result === undefined) {
            throw new Error(`No mock for board ${key}`);
          }
          return result;
        };
      }

      function makeGitHubProjectItem(
        repository: string,
        number: number,
        status: string,
        title = `Issue ${number}`
      ) {
        return {
          content: {
            number,
            repository,
            url: `https://github.com/${repository}/issues/${number}`,
            type: "Issue",
            title,
          },
          status,
          labels: [],
        };
      }

      it("fetches from primary board only when no extras configured", async () => {
        boardMocks = new Map([
          ["acme/123", [makeGitHubProjectItem("acme/widgets", 10, "Todo", "Primary issue")]],
        ]);
        await startTestServer({ legionId: "acme/123", fetchProjectItems: makeFetchProjectItems() });

        const response = await requestJson("/state/fetch-and-collect", {
          method: "POST",
          body: JSON.stringify({ backend: "github" }),
        });

        expect(response.status).toBe(200);
        const body = (await response.json()) as {
          issues: Record<string, { status: string }>;
          titles: Record<string, string>;
        };
        expect(body.issues["acme-widgets-10"]).toBeDefined();
        expect(body.titles["acme-widgets-10"]).toBe("Primary issue");
      });

      it("fetches from primary + extra boards and merges issues", async () => {
        boardMocks = new Map([
          ["acme/123", [makeGitHubProjectItem("acme/widgets", 10, "Todo", "Primary issue")]],
          ["acme/456", [makeGitHubProjectItem("other/repo", 5, "In Progress", "Extra issue")]],
        ]);
        await startTestServer({
          legionId: "acme/123",
          extraProjects: ["acme/456"],
          fetchProjectItems: makeFetchProjectItems(),
        });

        const response = await requestJson("/state/fetch-and-collect", {
          method: "POST",
          body: JSON.stringify({ backend: "github" }),
        });

        expect(response.status).toBe(200);
        const body = (await response.json()) as {
          issues: Record<string, { status: string }>;
          titles: Record<string, string>;
        };
        expect(body.issues["acme-widgets-10"]).toBeDefined();
        expect(body.issues["other-repo-5"]).toBeDefined();
        expect(body.titles["acme-widgets-10"]).toBe("Primary issue");
        expect(body.titles["other-repo-5"]).toBe("Extra issue");
      });

      it("deduplicates by canonical identity with primary board winning", async () => {
        const warnCalls: string[] = [];
        console.warn = (...args: unknown[]) => {
          warnCalls.push(args.map((arg) => String(arg)).join(" "));
        };
        boardMocks = new Map([
          ["acme/123", [makeGitHubProjectItem("acme/widgets", 42, "Todo", "Primary copy")]],
          [
            "acme/456",
            [makeGitHubProjectItem("acme/widgets", 42, "In Progress", "Secondary copy")],
          ],
        ]);
        await startTestServer({
          legionId: "acme/123",
          extraProjects: ["acme/456"],
          fetchProjectItems: makeFetchProjectItems(),
        });

        const response = await requestJson("/state/fetch-and-collect", {
          method: "POST",
          body: JSON.stringify({ backend: "github" }),
        });

        expect(response.status).toBe(200);
        const body = (await response.json()) as {
          issues: Record<string, { status: string }>;
          titles: Record<string, string>;
        };
        expect(Object.keys(body.issues)).toEqual(["acme-widgets-42"]);
        expect(body.issues["acme-widgets-42"]?.status).toBe("Todo");
        expect(body.titles["acme-widgets-42"]).toBe("Primary copy");
        expect(
          warnCalls.some(
            (message) =>
              /duplicate issue.*on boards/.test(message) && message.includes("acme/widgets#42")
          )
        ).toBe(true);
      });

      it("returns HTTP 200 when one extra board fails", async () => {
        const boardEntries: Array<[string, unknown]> = [
          ["acme/123", [makeGitHubProjectItem("acme/widgets", 10, "Todo", "Primary issue")]],
          ["acme/456", new Error("extra board unavailable")],
        ];
        boardMocks = new Map(boardEntries);
        await startTestServer({
          legionId: "acme/123",
          extraProjects: ["acme/456"],
          fetchProjectItems: makeFetchProjectItems(),
        });

        const response = await requestJson("/state/fetch-and-collect", {
          method: "POST",
          body: JSON.stringify({ backend: "github" }),
        });

        expect(response.status).toBe(200);
        const body = (await response.json()) as {
          issues: Record<string, { status: string }>;
        };
        expect(body.issues["acme-widgets-10"]).toBeDefined();
      });

      it("returns HTTP 500 when ALL boards fail", async () => {
        const boardEntries: Array<[string, unknown]> = [
          ["acme/123", new Error("primary failed")],
          ["acme/456", new Error("extra failed")],
        ];
        boardMocks = new Map(boardEntries);
        await startTestServer({
          legionId: "acme/123",
          extraProjects: ["acme/456"],
          fetchProjectItems: makeFetchProjectItems(),
        });

        const response = await requestJson("/state/fetch-and-collect", {
          method: "POST",
          body: JSON.stringify({ backend: "github" }),
        });

        expect(response.status).toBe(500);
        const body = (await response.json()) as { error: string };
        expect(body.error).toContain("fetch_and_collect_failed");
      });
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

  describe("per-worker env", () => {
    it("POST /workers stores env and GET /workers/{id}/env returns it", async () => {
      await startTestServer();
      const createRes = await requestJson("/workers", {
        method: "POST",
        body: JSON.stringify({
          issueId: "ENG-500",
          mode: "plan",
          workspace: "/tmp/work-500",
          env: { GH_TOKEN: "ghs_test", GIT_AUTHOR_NAME: "bot[bot]" },
        }),
      });
      expect(createRes.status).toBe(200);
      const created = (await createRes.json()) as { id: string };

      const envRes = await requestJson(`/workers/${created.id}/env`);
      expect(envRes.status).toBe(200);
      const envBody = (await envRes.json()) as { env: Record<string, string> };
      // Credential vars are stripped from the response
      expect(envBody.env).toEqual({});
    });

    it("GET /workers/{id}/env returns empty when no env set", async () => {
      await startTestServer();
      await requestJson("/workers", {
        method: "POST",
        body: JSON.stringify({
          issueId: "ENG-501",
          mode: "review",
          workspace: "/tmp/work-501",
        }),
      });

      const envRes = await requestJson("/workers/eng-501-review/env");
      expect(envRes.status).toBe(200);
      const envBody = (await envRes.json()) as { env: Record<string, string> };
      expect(envBody.env).toEqual({});
    });

    it("GET /workers list does not leak env", async () => {
      await startTestServer();
      await requestJson("/workers", {
        method: "POST",
        body: JSON.stringify({
          issueId: "ENG-502",
          mode: "implement",
          workspace: "/tmp/work-502",
          env: { SECRET: "should-not-leak", CUSTOM_VAR: "visible" },
        }),
      });

      const listRes = await requestJson("/workers");
      expect(listRes.status).toBe(200);
      const workers = (await listRes.json()) as Array<Record<string, unknown>>;
      for (const w of workers) {
        expect(w).not.toHaveProperty("env");
      }
    });

    it("GET /workers/{id} does not leak env", async () => {
      await startTestServer();
      await requestJson("/workers", {
        method: "POST",
        body: JSON.stringify({
          issueId: "ENG-503",
          mode: "implement",
          workspace: "/tmp/work-503",
          env: { SECRET: "should-not-leak" },
        }),
      });

      const detailRes = await requestJson("/workers/eng-503-implement");
      expect(detailRes.status).toBe(200);
      const body = (await detailRes.json()) as Record<string, unknown>;
      expect(body).not.toHaveProperty("env");
    });

    it("PATCH /workers/{id} response does not leak env", async () => {
      await startTestServer();
      await requestJson("/workers", {
        method: "POST",
        body: JSON.stringify({
          issueId: "ENG-504",
          mode: "implement",
          workspace: "/tmp/work-504",
          env: { SECRET: "should-not-leak" },
        }),
      });

      const patchRes = await requestJson("/workers/eng-504-implement", {
        method: "PATCH",
        body: JSON.stringify({ status: "stopped" }),
      });
      expect(patchRes.status).toBe(200);
      const body = (await patchRes.json()) as Record<string, unknown>;
      expect(body).not.toHaveProperty("env");
    });
  });

  it("GET /credentials/{role} endpoint is removed", async () => {
    await startTestServer();
    const res = await requestJson("/credentials/impl");
    expect(res.status).toBe(404);
  });

  it("GET /workers/{id}/token returns owner-scoped credentials", async () => {
    const tokenManager = createTestTokenManager({
      implement: {
        appId: "app-1",
        privateKey: "unused",
        installations: { acme: "111" },
      },
    });

    await startTestServer({
      tokenManager,
      state: {
        workers: {
          "eng-520-implement": {
            id: "eng-520-implement",
            port: sharedServePort,
            sessionId: "ses_eng_520",
            workspace: "/tmp/work-520",
            startedAt: "2026-02-01T00:00:00.000Z",
            status: "running",
            crashCount: 0,
            lastCrashAt: null,
            repo: "acme/widgets",
          },
        },
        crashHistory: {},
      },
    });

    const response = await requestJson("/workers/eng-520-implement/token");
    expect(response.status).toBe(200);
    const body = (await response.json()) as {
      role: string;
      owner: string;
      expiresAt: string;
      env: Record<string, string>;
    };

    expect(body).toEqual({
      role: "implement",
      owner: "acme",
      expiresAt: "2099-01-01T00:00:00.000Z",
      env: {
        GH_TOKEN: "ghs_owner_token:implement:acme",
        GIT_AUTHOR_NAME: "implement-bot[bot]",
        GIT_AUTHOR_EMAIL: "acme+implement@users.noreply.github.com",
        GIT_COMMITTER_NAME: "implement-bot[bot]",
        GIT_COMMITTER_EMAIL: "acme+implement@users.noreply.github.com",
        LEGION_APP_ROLE: "implement",
      },
    });
  });

  it("GET /workers/{id}/token rejects workers without repo", async () => {
    await startTestServer({
      tokenManager: createTestTokenManager({
        implement: {
          appId: "app-1",
          privateKey: "unused",
          installations: { acme: "111" },
        },
      }),
      state: {
        workers: {
          "eng-521-implement": {
            id: "eng-521-implement",
            port: sharedServePort,
            sessionId: "ses_eng_521",
            workspace: "/tmp/work-521",
            startedAt: "2026-02-01T00:00:00.000Z",
            status: "running",
            crashCount: 0,
            lastCrashAt: null,
          },
        },
        crashHistory: {},
      },
    });

    const response = await requestJson("/workers/eng-521-implement/token");
    expect(response.status).toBe(400);
    expect(await response.json()).toEqual({ error: "missing_repo" });
  });

  it("GET /workers/{id}/token returns 404 when token manager is unavailable", async () => {
    await startTestServer({
      state: {
        workers: {
          "eng-522-implement": {
            id: "eng-522-implement",
            port: sharedServePort,
            sessionId: "ses_eng_522",
            workspace: "/tmp/work-522",
            startedAt: "2026-02-01T00:00:00.000Z",
            status: "running",
            crashCount: 0,
            lastCrashAt: null,
            repo: "acme/widgets",
          },
        },
        crashHistory: {},
      },
    });

    const response = await requestJson("/workers/eng-522-implement/token");
    expect(response.status).toBe(404);
    expect(await response.json()).toEqual({ error: "token_manager_unavailable" });
  });

  it("GET /workers/{id}/token returns 404 for unconfigured role", async () => {
    await startTestServer({
      tokenManager: createTestTokenManager({
        review: {
          appId: "app-2",
          privateKey: "unused",
          installations: { acme: "222" },
        },
      }),
      state: {
        workers: {
          "eng-523-implement": {
            id: "eng-523-implement",
            port: sharedServePort,
            sessionId: "ses_eng_523",
            workspace: "/tmp/work-523",
            startedAt: "2026-02-01T00:00:00.000Z",
            status: "running",
            crashCount: 0,
            lastCrashAt: null,
            repo: "acme/widgets",
          },
        },
        crashHistory: {},
      },
    });

    const response = await requestJson("/workers/eng-523-implement/token");
    expect(response.status).toBe(404);
    expect(await response.json()).toEqual({ error: "role_not_configured" });
  });

  it("GET /workers/{id}/env strips credential vars from response", async () => {
    await startTestServer();
    await requestJson("/workers", {
      method: "POST",
      body: JSON.stringify({
        issueId: "ENG-510",
        mode: "implement",
        workspace: "/tmp/work-510",
        env: {
          GH_TOKEN: "ghs_secret",
          GIT_AUTHOR_NAME: "bot[bot]",
          GIT_AUTHOR_EMAIL: "bot@example.com",
          GIT_COMMITTER_NAME: "bot[bot]",
          GIT_COMMITTER_EMAIL: "bot@example.com",
          LEGION_APP_ROLE: "impl",
          CUSTOM_VAR: "visible",
        },
      }),
    });

    const envRes = await requestJson("/workers/eng-510-implement/env");
    expect(envRes.status).toBe(200);
    const envBody = (await envRes.json()) as { env: Record<string, string> };
    // Only non-credential vars should be returned
    expect(envBody.env).toEqual({ CUSTOM_VAR: "visible" });
    // Credential vars must not be present
    expect(envBody.env).not.toHaveProperty("GH_TOKEN");
    expect(envBody.env).not.toHaveProperty("GIT_AUTHOR_NAME");
    expect(envBody.env).not.toHaveProperty("LEGION_APP_ROLE");
  });

  it("POST /workers rejects non-string env values", async () => {
    await startTestServer();
    const res = await requestJson("/workers", {
      method: "POST",
      body: JSON.stringify({
        issueId: "ENG-511",
        mode: "implement",
        workspace: "/tmp/work-511",
        env: { GOOD: "string", BAD: 123 },
      }),
    });
    expect(res.status).toBe(400);
    const body = (await res.json()) as { error: string };
    expect(body.error).toContain('env values must be strings (key "BAD")');
  });

  describe("feedback logging", () => {
    it("emits worker.dispatched after POST /workers", async () => {
      const writer = new RecordingFeedbackWriter();
      const feedbackLogger = new FeedbackLogger(writer, legionId);

      await startTestServer({ feedbackLogger });

      const workspace = path.join(tempDir ?? os.tmpdir(), "workspace-one");
      const response = await requestJson("/workers", {
        method: "POST",
        body: JSON.stringify({
          issueId: "ENG-21",
          mode: "implement",
          workspace,
          version: 3,
        }),
      });

      expect(response.status).toBe(200);
      await feedbackLogger.flush();

      const events = parseFeedbackLines(writer.lines);
      expect(events).toHaveLength(1);
      const dispatched = events[0];
      expect(dispatched.schemaVersion).toBe(1);
      expect(dispatched.legionId).toBe(legionId);
      expect(dispatched.event).toBe("worker.dispatched");
      if (dispatched.event === "worker.dispatched") {
        expect(dispatched.issueId).toBe("eng-21");
        expect(dispatched.mode).toBe("implement");
        expect(dispatched.version).toBe(3);
        expect(dispatched.workspace).toBe(workspace);
      }
    });

    it("emits worker.status_changed after PATCH /workers/:id", async () => {
      const writer = new RecordingFeedbackWriter();
      const feedbackLogger = new FeedbackLogger(writer, legionId);

      await startTestServer({ feedbackLogger });

      const workspace = path.join(tempDir ?? os.tmpdir(), "workspace-two");
      const createResponse = await requestJson("/workers", {
        method: "POST",
        body: JSON.stringify({
          issueId: "ENG-22",
          mode: "test",
          workspace,
          version: 4,
        }),
      });
      const created = (await createResponse.json()) as { id: string };

      const patchResponse = await requestJson(`/workers/${created.id}`, {
        method: "PATCH",
        body: JSON.stringify({
          status: "running",
          crashCount: 0,
          lastCrashAt: null,
        }),
      });

      expect(patchResponse.status).toBe(200);
      await feedbackLogger.flush();

      const events = parseFeedbackLines(writer.lines);
      const statusEvent = events.find((event) => event.event === "worker.status_changed");
      expect(statusEvent).toBeDefined();
      expect(statusEvent?.schemaVersion).toBe(1);
      expect(statusEvent?.legionId).toBe(legionId);
      expect(statusEvent?.workerId).toBe(created.id);
      expect(statusEvent?.fromStatus).toBe("running");
      expect(statusEvent?.toStatus).toBe("running");
      expect(statusEvent?.version).toBe(4);
    });

    it("emits state.collected for each collected issue", async () => {
      const writer = new RecordingFeedbackWriter();
      const feedbackLogger = new FeedbackLogger(writer, legionId);

      await startTestServer({ feedbackLogger });

      const response = await requestJson("/state/collect", {
        method: "POST",
        body: JSON.stringify({
          backend: "linear",
          issues: [
            {
              identifier: "ENG-31",
              state: { name: "Todo" },
              labels: { nodes: [] },
            },
            {
              identifier: "ENG-32",
              state: { name: "In Progress" },
              labels: { nodes: [{ name: "worker-active" }] },
            },
          ],
        }),
      });

      expect(response.status).toBe(200);
      await feedbackLogger.flush();

      const events = parseFeedbackLines(writer.lines).filter(
        (event) => event.event === "state.collected"
      );
      expect(events).toHaveLength(2);
      expect(events[0].schemaVersion).toBe(1);
      expect(events[0].legionId).toBe(legionId);
      expect(events.map((event) => event.issueId)).toEqual(["ENG-31", "ENG-32"]);
    });

    it("does nothing when no feedbackLogger is provided", async () => {
      await startTestServer();

      const response = await requestJson("/state/collect", {
        method: "POST",
        body: JSON.stringify({
          backend: "linear",
          issues: [],
        }),
      });

      expect(response.status).toBe(200);
    });
  });

  describe("Envoy worker auto-subscribe", () => {
    const repoPaths: LegionPaths = {
      dataDir: "/tmp/legion-data",
      stateDir: "/tmp/legion-state",
      reposDir: "/tmp/legion-data/repos",
      workspacesDir: "/tmp/legion-data/workspaces",
      legionsFile: "/tmp/legion-state/legions.json",
      forLegion: (projectId: string) => ({
        legionStateDir: `/tmp/legion-state/legions/${projectId}`,
        workersFile: `/tmp/legion-state/legions/${projectId}/workers.json`,
        promotedFile: `/tmp/legion-state/legions/${projectId}/promoted.json`,
        feedbackFile: `/tmp/legion-state/legions/${projectId}/feedback.jsonl`,
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
      symlink: async () => {},
    };

    it("subscribes plan worker to Envoy issue topic when repo and issueNumber present", async () => {
      await startTestServer({ paths: repoPaths, repoManagerDeps });

      const response = await requestJson("/workers", {
        method: "POST",
        body: JSON.stringify({
          issueId: "acme-widgets-42",
          mode: "plan",
          repo: "acme/widgets",
          issueNumber: 42,
        }),
      });

      expect(response.status).toBe(200);
      const body = (await response.json()) as { sessionId: string };

      // Flush fire-and-forget microtasks
      await Bun.sleep(50);

      expect(mockEnvoy.subscribeCalls).toHaveLength(1);
      expect(mockEnvoy.subscribeCalls[0].session_id).toBe(body.sessionId);
      expect(mockEnvoy.subscribeCalls[0].topics).toEqual([
        "notifications.github.acme.widgets.issue.42.>",
      ]);
    });

    it("auto-extracts issueNumber from issueId and subscribes plan worker to Envoy", async () => {
      await startTestServer({ paths: repoPaths, repoManagerDeps });

      // Dispatch plan worker WITHOUT explicit issueNumber — matches real controller behavior
      const response = await requestJson("/workers", {
        method: "POST",
        body: JSON.stringify({
          issueId: "acme-widgets-44",
          mode: "plan",
          repo: "acme/widgets",
        }),
      });

      expect(response.status).toBe(200);
      const body = (await response.json()) as { sessionId: string };

      await Bun.sleep(50);

      // Should still subscribe — issueNumber auto-extracted from issueId
      expect(mockEnvoy.subscribeCalls).toHaveLength(1);
      expect(mockEnvoy.subscribeCalls[0].session_id).toBe(body.sessionId);
      expect(mockEnvoy.subscribeCalls[0].topics).toEqual([
        "notifications.github.acme.widgets.issue.44.>",
      ]);

      // Verify envoyTopics stored on worker entry
      const workerRes = await requestJson("/workers");
      const workers = (await workerRes.json()) as Array<{
        id: string;
        envoyTopics?: string[];
        issueNumber?: number;
      }>;
      const planWorker = workers.find((w) => w.id === "acme-widgets-44-plan");
      expect(planWorker?.envoyTopics).toEqual(["notifications.github.acme.widgets.issue.44.>"]);
      expect(planWorker?.issueNumber).toBe(44);
    });

    it("auto-extracts issueNumber from compound issueId with trailing slug", async () => {
      await startTestServer({ paths: repoPaths, repoManagerDeps });

      const response = await requestJson("/workers", {
        method: "POST",
        body: JSON.stringify({
          issueId: "acme-widgets-44-calendar-email-expense-review",
          mode: "implement",
          repo: "acme/widgets",
        }),
      });

      expect(response.status).toBe(200);

      const workerRes = await requestJson("/workers");
      const workers = (await workerRes.json()) as Array<{
        id: string;
        issueNumber?: number;
      }>;
      const worker = workers.find(
        (w) => w.id === "acme-widgets-44-calendar-email-expense-review-implement"
      );
      expect(worker?.issueNumber).toBe(44);
    });

    it("does not extract issueNumber when no leading digits after prefix", async () => {
      await startTestServer({ paths: repoPaths, repoManagerDeps });

      const response = await requestJson("/workers", {
        method: "POST",
        body: JSON.stringify({
          issueId: "acme-widgets-calendar",
          mode: "implement",
          repo: "acme/widgets",
        }),
      });

      expect(response.status).toBe(200);

      const workerRes = await requestJson("/workers");
      const workers = (await workerRes.json()) as Array<{
        id: string;
        issueNumber?: number;
      }>;
      const worker = workers.find((w) => w.id === "acme-widgets-calendar-implement");
      expect(worker?.issueNumber).toBeUndefined();
    });

    it("does not extract issueNumber when issue number is zero", async () => {
      await startTestServer({ paths: repoPaths, repoManagerDeps });

      const response = await requestJson("/workers", {
        method: "POST",
        body: JSON.stringify({
          issueId: "acme-widgets-0",
          mode: "implement",
          repo: "acme/widgets",
        }),
      });

      expect(response.status).toBe(200);

      const workerRes = await requestJson("/workers");
      const workers = (await workerRes.json()) as Array<{
        id: string;
        issueNumber?: number;
      }>;
      const worker = workers.find((w) => w.id === "acme-widgets-0-implement");
      expect(worker?.issueNumber).toBeUndefined();
    });

    it("does not extract issueNumber when digits are followed by letters", async () => {
      await startTestServer({ paths: repoPaths, repoManagerDeps });

      const response = await requestJson("/workers", {
        method: "POST",
        body: JSON.stringify({
          issueId: "acme-widgets-123abc",
          mode: "implement",
          repo: "acme/widgets",
        }),
      });

      expect(response.status).toBe(200);

      const workerRes = await requestJson("/workers");
      const workers = (await workerRes.json()) as Array<{
        id: string;
        issueNumber?: number;
      }>;
      const worker = workers.find((w) => w.id === "acme-widgets-123abc-implement");
      expect(worker?.issueNumber).toBeUndefined();
    });

    it("skips Envoy subscribe when issueNumber is absent", async () => {
      await startTestServer({ paths: repoPaths, repoManagerDeps });

      const response = await requestJson("/workers", {
        method: "POST",
        body: JSON.stringify({
          issueId: "acme-widgets-43",
          mode: "implement",
          repo: "acme/widgets",
        }),
      });

      expect(response.status).toBe(200);
      await Bun.sleep(50);
      expect(mockEnvoy.subscribeCalls).toHaveLength(0);
    });

    it("skips Envoy subscribe when repo is absent (workspace mode)", async () => {
      await startTestServer();

      const response = await requestJson("/workers", {
        method: "POST",
        body: JSON.stringify({
          issueId: "ENG-99",
          mode: "implement",
          workspace: "/tmp/legacy-workspace",
          issueNumber: 99,
        }),
      });

      expect(response.status).toBe(200);
      await Bun.sleep(50);
      expect(mockEnvoy.subscribeCalls).toHaveLength(0);
    });

    it("returns 200 even when Envoy subscribe fails with HTTP 500", async () => {
      mockEnvoy.subscribeStatus = 500;

      await startTestServer({ paths: repoPaths, repoManagerDeps });

      const response = await requestJson("/workers", {
        method: "POST",
        body: JSON.stringify({
          issueId: "acme-widgets-44",
          mode: "plan",
          repo: "acme/widgets",
          issueNumber: 44,
        }),
      });

      expect(response.status).toBe(200);
      await Bun.sleep(50);
      expect(mockEnvoy.subscribeCalls).toHaveLength(1);
    });

    it("returns 200 even when Envoy subscribe fails with network error", async () => {
      // Stop the mock server so fetch() genuinely fails with a network error
      const deadPort = mockEnvoy.url.split(":").pop();
      mockEnvoy.stop();

      await startTestServer({
        paths: repoPaths,
        repoManagerDeps,
        envoyUrl: `http://127.0.0.1:${deadPort}`,
      });

      const response = await requestJson("/workers", {
        method: "POST",
        body: JSON.stringify({
          issueId: "acme-widgets-45",
          mode: "plan",
          repo: "acme/widgets",
          issueNumber: 45,
        }),
      });

      expect(response.status).toBe(200);
      await Bun.sleep(50);
    });

    it("accepts optional issueNumber without changing existing response shape", async () => {
      await startTestServer();

      const response = await requestJson("/workers", {
        method: "POST",
        body: JSON.stringify({
          issueId: "ENG-55",
          mode: "implement",
          workspace: "/tmp/work",
          issueNumber: 55,
        }),
      });

      expect(response.status).toBe(200);
      const body = (await response.json()) as { id: string; port: number; sessionId: string };
      expect(body.id).toBe("eng-55-implement");
      expect(body.port).toBe(sharedServePort);
      expect(typeof body.sessionId).toBe("string");
    });

    it("unsubscribes worker from envoy on delete", async () => {
      await startTestServer({ paths: repoPaths, repoManagerDeps });

      const createResponse = await requestJson("/workers", {
        method: "POST",
        body: JSON.stringify({
          issueId: "acme-widgets-250",
          mode: "implement",
          repo: "acme/widgets",
          issueNumber: 250,
        }),
      });
      expect(createResponse.status).toBe(200);
      const created = (await createResponse.json()) as { id: string; sessionId: string };
      await Bun.sleep(50);

      mockEnvoy.subscribeCalls.length = 0;
      mockEnvoy.unsubscribeCalls.length = 0;

      const deleteResponse = await requestJson(`/workers/${created.id}`, { method: "DELETE" });
      expect(deleteResponse.status).toBe(200);

      await Bun.sleep(50);

      const unsubCalls = mockEnvoy.unsubscribeCalls;
      expect(unsubCalls).toHaveLength(1);
      expect(unsubCalls[0]).toEqual(
        expect.objectContaining({
          session_id: created.sessionId,
          topics: [],
        })
      );
    });

    it("delete succeeds even when envoy unsubscribe fails", async () => {
      mockEnvoy.unsubscribeStatus = 500;

      await startTestServer({ paths: repoPaths, repoManagerDeps });

      const createResponse = await requestJson("/workers", {
        method: "POST",
        body: JSON.stringify({
          issueId: "acme-widgets-251",
          mode: "implement",
          repo: "acme/widgets",
          issueNumber: 251,
        }),
      });
      expect(createResponse.status).toBe(200);
      const created = (await createResponse.json()) as { id: string };

      const deleteResponse = await requestJson(`/workers/${created.id}`, { method: "DELETE" });
      expect(deleteResponse.status).toBe(200);
      expect(await deleteResponse.json()).toEqual({ status: "stopped" });
    });

    it("does not subscribe non-plan modes to Envoy even with repo and issueNumber", async () => {
      await startTestServer({ paths: repoPaths, repoManagerDeps });

      for (const mode of ["implement", "test", "review", "architect"]) {
        const response = await requestJson("/workers", {
          method: "POST",
          body: JSON.stringify({
            issueId: `acme-widgets-mode-${mode}`,
            mode,
            repo: "acme/widgets",
            issueNumber: 100,
          }),
        });
        expect(response.status).toBe(200);
      }

      // Also test merge (gated mode, requires force)
      const mergeResponse = await requestJson("/workers", {
        method: "POST",
        body: JSON.stringify({
          issueId: "acme-widgets-mode-merge",
          mode: "merge",
          repo: "acme/widgets",
          issueNumber: 100,
          force: true,
        }),
      });
      expect(mergeResponse.status).toBe(200);

      await Bun.sleep(50);

      const issueSubscribeCalls = mockEnvoy.subscribeCalls.filter((c) =>
        c.topics.some((topic) => topic.includes(".issue."))
      );
      expect(issueSubscribeCalls).toHaveLength(0);
    });

    it("unsubscribes existing plan worker when dispatching implement for same issue", async () => {
      await startTestServer({ paths: repoPaths, repoManagerDeps });

      // Dispatch plan worker
      const planResponse = await requestJson("/workers", {
        method: "POST",
        body: JSON.stringify({
          issueId: "acme-widgets-60",
          mode: "plan",
          repo: "acme/widgets",
          issueNumber: 60,
        }),
      });
      expect(planResponse.status).toBe(200);
      const planWorker = (await planResponse.json()) as { sessionId: string };
      await Bun.sleep(50);

      // Verify plan was subscribed
      const subscribeCalls = mockEnvoy.subscribeCalls;
      expect(subscribeCalls).toHaveLength(1);
      expect(subscribeCalls[0].session_id).toBe(planWorker.sessionId);

      // Reset tracking
      mockEnvoy.subscribeCalls.length = 0;
      mockEnvoy.unsubscribeCalls.length = 0;

      // Dispatch implement worker for same issue
      const implResponse = await requestJson("/workers", {
        method: "POST",
        body: JSON.stringify({
          issueId: "acme-widgets-60",
          mode: "implement",
          repo: "acme/widgets",
          issueNumber: 60,
        }),
      });
      expect(implResponse.status).toBe(200);
      await Bun.sleep(50);

      // Plan worker should be unsubscribed, implement should NOT be subscribed
      const unsubCalls = mockEnvoy.unsubscribeCalls;
      expect(unsubCalls).toHaveLength(1);
      expect(unsubCalls[0].session_id).toBe(planWorker.sessionId);

      const issueSubscribeCalls = mockEnvoy.subscribeCalls.filter((c) =>
        c.topics.some((topic) => topic.includes(".issue."))
      );
      expect(issueSubscribeCalls).toHaveLength(0);
    });

    it("includes envoyTopics in GET /workers for plan mode dispatch", async () => {
      await startTestServer({ paths: repoPaths, repoManagerDeps });

      await requestJson("/workers", {
        method: "POST",
        body: JSON.stringify({
          issueId: "acme-widgets-70",
          mode: "plan",
          repo: "acme/widgets",
          issueNumber: 70,
        }),
      });

      const listResponse = await requestJson("/workers");
      expect(listResponse.status).toBe(200);
      const workers = (await listResponse.json()) as Array<{
        id: string;
        envoyTopics?: string[];
      }>;
      const planWorker = workers.find((w) => w.id === "acme-widgets-70-plan");
      expect(planWorker).toBeDefined();
      expect(planWorker?.envoyTopics).toEqual(["notifications.github.acme.widgets.issue.70.>"]);
    });

    it("does not subscribe planner to PR topics", async () => {
      await startTestServer({ paths: repoPaths, repoManagerDeps });

      await requestJson("/workers", {
        method: "POST",
        body: JSON.stringify({
          issueId: "acme-widgets-72",
          mode: "plan",
          repo: "acme/widgets",
          issueNumber: 72,
        }),
      });
      await Bun.sleep(50);

      const subscribeCalls = mockEnvoy.subscribeCalls;
      expect(subscribeCalls).toHaveLength(1);
      const topics = subscribeCalls[0].topics;
      expect(topics).toEqual(["notifications.github.acme.widgets.issue.72.>"]);
      // Explicitly verify no PR topic
      expect(topics.some((t) => t.includes(".pr."))).toBe(false);
    });

    it("omits envoyTopics for non-plan mode dispatch", async () => {
      await startTestServer({ paths: repoPaths, repoManagerDeps });

      await requestJson("/workers", {
        method: "POST",
        body: JSON.stringify({
          issueId: "acme-widgets-71",
          mode: "implement",
          repo: "acme/widgets",
          issueNumber: 71,
        }),
      });

      const listResponse = await requestJson("/workers");
      const workers = (await listResponse.json()) as Array<{
        id: string;
        envoyTopics?: string[];
      }>;
      const implWorker = workers.find((w) => w.id === "acme-widgets-71-implement");
      expect(implWorker).toBeDefined();
      expect(implWorker?.envoyTopics).toBeUndefined();
    });

    it("unsubscribes and clears envoyTopics when worker status changes to dead", async () => {
      await startTestServer({ paths: repoPaths, repoManagerDeps });

      // Dispatch plan worker (gets envoyTopics)
      const createResponse = await requestJson("/workers", {
        method: "POST",
        body: JSON.stringify({
          issueId: "acme-widgets-80",
          mode: "plan",
          repo: "acme/widgets",
          issueNumber: 80,
        }),
      });
      expect(createResponse.status).toBe(200);
      const created = (await createResponse.json()) as { id: string; sessionId: string };
      await Bun.sleep(50);

      // Reset tracking
      mockEnvoy.subscribeCalls.length = 0;
      mockEnvoy.unsubscribeCalls.length = 0;

      // PATCH status to dead
      const patchResponse = await requestJson(`/workers/${created.id}`, {
        method: "PATCH",
        body: JSON.stringify({
          status: "dead",
          crashCount: 1,
          lastCrashAt: new Date().toISOString(),
        }),
      });
      expect(patchResponse.status).toBe(200);
      await Bun.sleep(50);

      // Should trigger unsubscribe
      const unsubCalls = mockEnvoy.unsubscribeCalls;
      expect(unsubCalls).toHaveLength(1);
      expect(unsubCalls[0].session_id).toBe(created.sessionId);

      // envoyTopics should be cleared
      const getResponse = await requestJson(`/workers/${created.id}`);
      expect(getResponse.status).toBe(200);
      const worker = (await getResponse.json()) as { envoyTopics?: string[] };
      expect(worker.envoyTopics).toBeUndefined();
    });

    it("cross-mode cleanup clears envoyTopics on old worker", async () => {
      await startTestServer({ paths: repoPaths, repoManagerDeps });

      // Dispatch plan worker
      await requestJson("/workers", {
        method: "POST",
        body: JSON.stringify({
          issueId: "acme-widgets-90",
          mode: "plan",
          repo: "acme/widgets",
          issueNumber: 90,
        }),
      });

      // Verify plan has envoyTopics
      let listResponse = await requestJson("/workers");
      let workers = (await listResponse.json()) as Array<{
        id: string;
        envoyTopics?: string[];
      }>;
      let planWorker = workers.find((w) => w.id === "acme-widgets-90-plan");
      expect(planWorker?.envoyTopics).toBeDefined();

      // Dispatch implement for same issue
      await requestJson("/workers", {
        method: "POST",
        body: JSON.stringify({
          issueId: "acme-widgets-90",
          mode: "implement",
          repo: "acme/widgets",
          issueNumber: 90,
        }),
      });

      // Plan worker's envoyTopics should be cleared
      listResponse = await requestJson("/workers");
      workers = (await listResponse.json()) as Array<{
        id: string;
        envoyTopics?: string[];
      }>;
      planWorker = workers.find((w) => w.id === "acme-widgets-90-plan");
      expect(planWorker?.envoyTopics).toBeUndefined();
    });

    it("does not unsubscribe workers for a different issue on cross-mode dispatch", async () => {
      await startTestServer({ paths: repoPaths, repoManagerDeps });

      // Dispatch plan worker for issue 50
      await requestJson("/workers", {
        method: "POST",
        body: JSON.stringify({
          issueId: "acme-widgets-50",
          mode: "plan",
          repo: "acme/widgets",
          issueNumber: 50,
        }),
      });
      await Bun.sleep(50);
      mockEnvoy.subscribeCalls.length = 0;
      mockEnvoy.unsubscribeCalls.length = 0;

      // Dispatch implement worker for issue 51 (different issue)
      await requestJson("/workers", {
        method: "POST",
        body: JSON.stringify({
          issueId: "acme-widgets-51",
          mode: "implement",
          repo: "acme/widgets",
          issueNumber: 51,
        }),
      });
      await Bun.sleep(50);

      // No unsubscribe calls — issue 50's plan worker untouched
      const unsubCalls = mockEnvoy.unsubscribeCalls;
      expect(unsubCalls).toHaveLength(0);
    });

    it("re-subscribes worker to issue topics on resume via prompt", async () => {
      await startTestServer({
        paths: repoPaths,
        repoManagerDeps,
      });

      // First create a worker
      const createResponse = await requestJson("/workers", {
        method: "POST",
        body: JSON.stringify({
          issueId: "acme-widgets-305",
          mode: "plan",
          repo: "acme/widgets",
          issueNumber: 305,
        }),
      });
      expect(createResponse.status).toBe(200);
      const created = (await createResponse.json()) as { id: string; sessionId: string };
      await Bun.sleep(50);

      // Clear calls from dispatch
      mockEnvoy.subscribeCalls.length = 0;
      mockEnvoy.unsubscribeCalls.length = 0;

      // Resume worker via prompt
      const promptResponse = await requestJson(`/workers/${created.id}/prompt`, {
        method: "POST",
        body: JSON.stringify({ text: "Address review comments" }),
      });
      expect(promptResponse.status).toBe(200);
      await Bun.sleep(50);

      // Should have re-subscribed to issue topics
      const issueSubs = mockEnvoy.subscribeCalls.filter((c) =>
        c.topics.some((t: string) => t.includes("issue.305"))
      );
      expect(issueSubs).toHaveLength(1);
      expect(issueSubs[0].session_id).toBe(created.sessionId);
      expect(issueSubs[0].topics).toEqual(["notifications.github.acme.widgets.issue.305.>"]);
    });

    it("skips resume re-subscription for workspace-only workers", async () => {
      await startTestServer();

      // Create a workspace-only worker (no repo/issueNumber)
      const createResponse = await requestJson("/workers", {
        method: "POST",
        body: JSON.stringify({
          issueId: "ENG-306",
          mode: "implement",
          workspace: "/tmp/work-306",
        }),
      });
      expect(createResponse.status).toBe(200);
      const created = (await createResponse.json()) as { id: string };
      await Bun.sleep(50);

      mockEnvoy.subscribeCalls.length = 0;

      // Resume worker via prompt
      const promptResponse = await requestJson(`/workers/${created.id}/prompt`, {
        method: "POST",
        body: JSON.stringify({ text: "Continue work" }),
      });
      expect(promptResponse.status).toBe(200);
      await Bun.sleep(50);

      // No subscribe calls — workspace-only worker has no repo/issueNumber
      expect(mockEnvoy.subscribeCalls).toHaveLength(0);
    });

    it("persists repo and issueNumber in worker entry", async () => {
      await startTestServer({ paths: repoPaths, repoManagerDeps });

      await requestJson("/workers", {
        method: "POST",
        body: JSON.stringify({
          issueId: "acme-widgets-307",
          mode: "implement",
          repo: "acme/widgets",
          issueNumber: 307,
        }),
      });

      // Read the state file to verify repo/issueNumber were persisted
      const stateContent = await readFile(path.join(tempDir ?? "", "workers.json"), "utf-8");
      const state = JSON.parse(stateContent) as PersistedWorkerState & {
        workers: Record<string, { repo?: string; issueNumber?: number }>;
      };
      const worker = Object.values(state.workers)[0];
      expect(worker.repo).toBe("acme/widgets");
      expect(worker.issueNumber).toBe(307);
    });

    it("prune unsubscribes workers from Envoy on Done cleanup", async () => {
      await startTestServer({ paths: repoPaths, repoManagerDeps });

      // Dispatch plan worker (gets daemon-managed envoyTopics)
      const planRes = await requestJson("/workers", {
        method: "POST",
        body: JSON.stringify({
          issueId: "acme-widgets-400",
          mode: "plan",
          repo: "acme/widgets",
          issueNumber: 400,
        }),
      });
      const planWorker = (await planRes.json()) as { sessionId: string };
      await Bun.sleep(50);

      // Reset tracking
      mockEnvoy.subscribeCalls.length = 0;
      mockEnvoy.unsubscribeCalls.length = 0;

      // Prune the issue (simulates Done cleanup)
      const pruneRes = await requestJson("/workers/prune", {
        method: "POST",
        body: JSON.stringify({ issueIds: ["acme-widgets-400"] }),
      });
      expect(pruneRes.status).toBe(200);
      await Bun.sleep(50);

      // detachWorkerFromEnvoy fires targeted unsubscribe for daemon-managed topics,
      // unsubscribeAllWorkerTopics fires blanket unsubscribe for self-managed topics.
      // Plan worker has envoyTopics, so both fire (2 calls).
      const unsubCalls = mockEnvoy.unsubscribeCalls;
      expect(unsubCalls.length).toBeGreaterThanOrEqual(1);
      expect(unsubCalls.every((c) => c.session_id === planWorker.sessionId)).toBe(true);
    });

    it("prune blanket-unsubscribes workers without daemon-managed topics", async () => {
      await startTestServer({ paths: repoPaths, repoManagerDeps });

      // Dispatch implement worker (no daemon-managed envoyTopics)
      const implRes = await requestJson("/workers", {
        method: "POST",
        body: JSON.stringify({
          issueId: "acme-widgets-401",
          mode: "implement",
          repo: "acme/widgets",
          issueNumber: 401,
        }),
      });
      const implWorker = (await implRes.json()) as { sessionId: string };
      await Bun.sleep(50);

      // Reset tracking
      mockEnvoy.subscribeCalls.length = 0;
      mockEnvoy.unsubscribeCalls.length = 0;

      // Prune the issue
      const pruneRes = await requestJson("/workers/prune", {
        method: "POST",
        body: JSON.stringify({ issueIds: ["acme-widgets-401"] }),
      });
      expect(pruneRes.status).toBe(200);
      await Bun.sleep(50);

      // Blanket unsubscribe catches self-managed subscriptions (e.g. PR topics)
      const unsubCalls = mockEnvoy.unsubscribeCalls;
      expect(unsubCalls).toHaveLength(1);
      expect(unsubCalls[0].session_id).toBe(implWorker.sessionId);
    });
  });

  describe("dispatch validation", () => {
    it("rejects gated mode dispatch when cache says wrong phase", async () => {
      await startTestServer();

      globalThis.fetch = (async (input: string | URL | Request, init?: RequestInit) => {
        const url =
          typeof input === "string" ? input : input instanceof URL ? input.href : input.url;
        if (url.includes(baseUrl)) {
          return originalFetch(input, init);
        }
        return new Response(JSON.stringify([]), { status: 200 });
      }) as typeof fetch;

      // Populate cache with a Needs Review issue (suggestedAction: transition_to_retro)
      await requestJson("/state/collect", {
        method: "POST",
        body: JSON.stringify({
          backend: "github",
          issues: [
            {
              content: {
                number: 42,
                repository: "test/repo",
                url: "https://github.com/test/repo/issues/42",
                type: "Issue",
              },
              status: "Needs Review",
              labels: ["worker-done"],
            },
          ],
        }),
      });

      // Try to dispatch merge — should be rejected
      const res = await requestJson("/workers", {
        method: "POST",
        body: JSON.stringify({
          issueId: "test-repo-42",
          mode: "merge",
          workspace: "/tmp/work",
        }),
      });

      expect(res.status).toBe(422);
      const body = (await res.json()) as { error: string; suggestedAction: string };
      expect(body.error).toBe("phase_prerequisite_unmet");
      expect(body.suggestedAction).toBeTruthy();
    });

    it("allows gated mode dispatch when cache says correct phase", async () => {
      await startTestServer();

      globalThis.fetch = (async (input: string | URL | Request, init?: RequestInit) => {
        const url =
          typeof input === "string" ? input : input instanceof URL ? input.href : input.url;
        if (url.includes(baseUrl)) {
          return originalFetch(input, init);
        }
        return new Response(JSON.stringify([]), { status: 200 });
      }) as typeof fetch;

      // Populate cache with Retro + worker-done (suggestedAction: dispatch_merger)
      await requestJson("/state/collect", {
        method: "POST",
        body: JSON.stringify({
          backend: "github",
          issues: [
            {
              content: {
                number: 43,
                repository: "test/repo",
                url: "https://github.com/test/repo/issues/43",
                type: "Issue",
              },
              status: "Retro",
              labels: ["worker-done"],
            },
          ],
        }),
      });

      // Dispatch merge — should succeed
      const res = await requestJson("/workers", {
        method: "POST",
        body: JSON.stringify({
          issueId: "test-repo-43",
          mode: "merge",
          workspace: "/tmp/work",
        }),
      });

      expect(res.status).toBe(200);
    });

    it("allows gated mode dispatch on cache miss (no prior collect)", async () => {
      await startTestServer();

      const res = await requestJson("/workers", {
        method: "POST",
        body: JSON.stringify({
          issueId: "unknown-issue-99",
          mode: "merge",
          workspace: "/tmp/work",
        }),
      });

      expect(res.status).toBe(200);
    });

    it("allows non-gated modes regardless of cached state", async () => {
      await startTestServer();

      const res = await requestJson("/workers", {
        method: "POST",
        body: JSON.stringify({
          issueId: "test-issue-1",
          mode: "implement",
          workspace: "/tmp/work",
        }),
      });

      expect(res.status).toBe(200);
    });

    it("bypasses validation when force flag is set", async () => {
      await startTestServer();

      globalThis.fetch = (async (input: string | URL | Request, init?: RequestInit) => {
        const url =
          typeof input === "string" ? input : input instanceof URL ? input.href : input.url;
        if (url.includes(baseUrl)) {
          return originalFetch(input, init);
        }
        return new Response(JSON.stringify([]), { status: 200 });
      }) as typeof fetch;

      // Populate cache with wrong phase for merge
      await requestJson("/state/collect", {
        method: "POST",
        body: JSON.stringify({
          backend: "github",
          issues: [
            {
              content: {
                number: 44,
                repository: "test/repo",
                url: "https://github.com/test/repo/issues/44",
                type: "Issue",
              },
              status: "Needs Review",
              labels: ["worker-done"],
            },
          ],
        }),
      });

      // Force dispatch merge — should succeed
      const res = await requestJson("/workers", {
        method: "POST",
        body: JSON.stringify({
          issueId: "test-repo-44",
          mode: "merge",
          workspace: "/tmp/work",
          force: true,
        }),
      });

      expect(res.status).toBe(200);
    });

    it("422 error includes attempted mode and suggestedAction", async () => {
      await startTestServer();

      globalThis.fetch = (async (input: string | URL | Request, init?: RequestInit) => {
        const url =
          typeof input === "string" ? input : input instanceof URL ? input.href : input.url;
        if (url.includes(baseUrl)) {
          return originalFetch(input, init);
        }
        return new Response(JSON.stringify([]), { status: 200 });
      }) as typeof fetch;

      // Populate cache
      await requestJson("/state/collect", {
        method: "POST",
        body: JSON.stringify({
          backend: "github",
          issues: [
            {
              content: {
                number: 45,
                repository: "test/repo",
                url: "https://github.com/test/repo/issues/45",
                type: "Issue",
              },
              status: "Needs Review",
              labels: ["worker-done"],
            },
          ],
        }),
      });

      const res = await requestJson("/workers", {
        method: "POST",
        body: JSON.stringify({
          issueId: "test-repo-45",
          mode: "merge",
          workspace: "/tmp/work",
        }),
      });

      expect(res.status).toBe(422);
      const body = (await res.json()) as {
        error: string;
        attemptedMode: string;
        suggestedAction: string;
        reason: string;
      };
      expect(body.error).toBe("phase_prerequisite_unmet");
      expect(body.attemptedMode).toBe("merge");
      expect(body.suggestedAction).toBeTruthy();
      expect(body.reason).toBeTruthy();
    });

    it("updates cache on subsequent collect calls (latest wins)", async () => {
      await startTestServer();

      globalThis.fetch = (async (input: string | URL | Request, init?: RequestInit) => {
        const url =
          typeof input === "string" ? input : input instanceof URL ? input.href : input.url;
        if (url.includes(baseUrl)) {
          return originalFetch(input, init);
        }
        return new Response(JSON.stringify([]), { status: 200 });
      }) as typeof fetch;

      // First collect: Needs Review + worker-done → transition_to_retro
      await requestJson("/state/collect", {
        method: "POST",
        body: JSON.stringify({
          backend: "github",
          issues: [
            {
              content: {
                number: 50,
                repository: "test/repo",
                url: "https://github.com/test/repo/issues/50",
                type: "Issue",
              },
              status: "Needs Review",
              labels: ["worker-done"],
            },
          ],
        }),
      });

      // merge should fail with stale cache
      const res1 = await requestJson("/workers", {
        method: "POST",
        body: JSON.stringify({
          issueId: "test-repo-50",
          mode: "merge",
          workspace: "/tmp/work",
        }),
      });
      expect(res1.status).toBe(422);

      // Second collect: Retro + worker-done → dispatch_merger
      await requestJson("/state/collect", {
        method: "POST",
        body: JSON.stringify({
          backend: "github",
          issues: [
            {
              content: {
                number: 50,
                repository: "test/repo",
                url: "https://github.com/test/repo/issues/50",
                type: "Issue",
              },
              status: "Retro",
              labels: ["worker-done"],
            },
          ],
        }),
      });

      // merge should now succeed with updated cache
      const res2 = await requestJson("/workers", {
        method: "POST",
        body: JSON.stringify({
          issueId: "test-repo-50",
          mode: "merge",
          workspace: "/tmp/work",
        }),
      });
      expect(res2.status).toBe(200);
    });

    it("cache lookup uses normalized issue ID (case insensitive)", async () => {
      await startTestServer();

      globalThis.fetch = (async (input: string | URL | Request, init?: RequestInit) => {
        const url =
          typeof input === "string" ? input : input instanceof URL ? input.href : input.url;
        if (url.includes(baseUrl)) {
          return originalFetch(input, init);
        }
        return new Response(JSON.stringify([]), { status: 200 });
      }) as typeof fetch;

      // Populate cache — issue ID from GitHub will be lowercase
      await requestJson("/state/collect", {
        method: "POST",
        body: JSON.stringify({
          backend: "github",
          issues: [
            {
              content: {
                number: 51,
                repository: "Test/Repo",
                url: "https://github.com/Test/Repo/issues/51",
                type: "Issue",
              },
              status: "Needs Review",
              labels: ["worker-done"],
            },
          ],
        }),
      });

      // Dispatch with mixed-case issueId — should still hit cache and reject
      const res = await requestJson("/workers", {
        method: "POST",
        body: JSON.stringify({
          issueId: "Test-Repo-51",
          mode: "merge",
          workspace: "/tmp/work",
        }),
      });

      expect(res.status).toBe(422);
    });
  });

  describe("POST /workers with prompt (#237)", () => {
    it("delivers prompt after session creation delay", async () => {
      const sendPromptCalls: Array<{ sessionId: string; text: string }> = [];
      await startTestServer({
        adapterOverrides: {
          sendPrompt: async (sessionId, text) => {
            sendPromptCalls.push({ sessionId, text });
          },
        },
      });

      const response = await requestJson("/workers", {
        method: "POST",
        body: JSON.stringify({
          issueId: "ENG-237",
          mode: "implement",
          workspace: "/tmp/work-237",
          prompt: "/legion-worker implement mode",
        }),
      });

      expect(response.status).toBe(200);
      const body = (await response.json()) as {
        id: string;
        sessionId: string;
        promptDelivered: boolean;
      };
      expect(body.id).toBe("eng-237-implement");
      expect(body.promptDelivered).toBe(true);
      expect(sendPromptCalls).toHaveLength(1);
      expect(sendPromptCalls[0].sessionId).toBe(body.sessionId);
      expect(sendPromptCalls[0].text).toBe("/legion-worker implement mode");
    });

    it("omits promptDelivered when no prompt provided", async () => {
      await startTestServer();

      const response = await requestJson("/workers", {
        method: "POST",
        body: JSON.stringify({
          issueId: "ENG-238",
          mode: "implement",
          workspace: "/tmp/work-238",
        }),
      });

      expect(response.status).toBe(200);
      const body = (await response.json()) as Record<string, unknown>;
      expect(body).not.toHaveProperty("promptDelivered");
    });

    it("returns promptDelivered=false when sendPrompt fails all retries", async () => {
      await startTestServer({
        adapterOverrides: {
          sendPrompt: async () => {
            throw new Error("session not bootstrapped");
          },
        },
      });

      const response = await requestJson("/workers", {
        method: "POST",
        body: JSON.stringify({
          issueId: "ENG-239",
          mode: "implement",
          workspace: "/tmp/work-239",
          prompt: "/legion-worker implement mode",
        }),
      });

      expect(response.status).toBe(200);
      const body = (await response.json()) as {
        id: string;
        promptDelivered: boolean;
      };
      expect(body.id).toBe("eng-239-implement");
      expect(body.promptDelivered).toBe(false);
    });

    it("retries prompt delivery on transient failures", async () => {
      let sendPromptAttempts = 0;
      await startTestServer({
        adapterOverrides: {
          sendPrompt: async (_sessionId, _text) => {
            sendPromptAttempts++;
            if (sendPromptAttempts < 3) {
              throw new Error("transient failure");
            }
          },
        },
      });

      const response = await requestJson("/workers", {
        method: "POST",
        body: JSON.stringify({
          issueId: "ENG-240",
          mode: "implement",
          workspace: "/tmp/work-240",
          prompt: "start working",
        }),
      });

      expect(response.status).toBe(200);
      const body = (await response.json()) as { promptDelivered: boolean };
      expect(body.promptDelivered).toBe(true);
      expect(sendPromptAttempts).toBe(3);
    });

    it("ignores empty string prompt", async () => {
      const sendPromptCalls: Array<{ sessionId: string; text: string }> = [];
      await startTestServer({
        adapterOverrides: {
          sendPrompt: async (sessionId, text) => {
            sendPromptCalls.push({ sessionId, text });
          },
        },
      });

      const response = await requestJson("/workers", {
        method: "POST",
        body: JSON.stringify({
          issueId: "ENG-241",
          mode: "implement",
          workspace: "/tmp/work-241",
          prompt: "",
        }),
      });

      expect(response.status).toBe(200);
      const body = (await response.json()) as Record<string, unknown>;
      expect(body).not.toHaveProperty("promptDelivered");
      expect(sendPromptCalls).toHaveLength(0);
    });
  });

  describe("POST /workers/prune", () => {
    it("prunes all workers for given issue IDs", async () => {
      await startTestServer();
      // Create workers for two issues with multiple modes
      await requestJson("/workers", {
        method: "POST",
        body: JSON.stringify({ issueId: "ENG-100", mode: "implement", workspace: "/tmp/w1" }),
      });
      await requestJson("/workers", {
        method: "POST",
        body: JSON.stringify({ issueId: "ENG-100", mode: "review", workspace: "/tmp/w2" }),
      });
      await requestJson("/workers", {
        method: "POST",
        body: JSON.stringify({ issueId: "ENG-200", mode: "implement", workspace: "/tmp/w3" }),
      });

      const pruneResponse = await requestJson("/workers/prune", {
        method: "POST",
        body: JSON.stringify({ issueIds: ["ENG-100"] }),
      });
      expect(pruneResponse.status).toBe(200);
      const pruneBody = (await pruneResponse.json()) as {
        pruned: string[];
        crashHistoryPruned: string[];
      };
      expect(pruneBody.pruned.sort()).toEqual(["eng-100-implement", "eng-100-review"]);

      // ENG-200 worker should still exist
      const listResponse = await requestJson("/workers");
      const workers = (await listResponse.json()) as WorkerEntry[];
      expect(workers).toHaveLength(1);
      expect(workers[0].id).toBe("eng-200-implement");

      // Sessions should be deleted from serve to release SQLite FDs
      // ENG-100 had 2 workers (implement + review), ENG-200 was not pruned
      expect(deleteSessionCalls).toHaveLength(2);
    });

    it("prunes crash history for matching workers", async () => {
      await startTestServer();
      // Create worker, mark dead with max crashes (creates crash history entry)
      const createResponse = await requestJson("/workers", {
        method: "POST",
        body: JSON.stringify({ issueId: "ENG-300", mode: "implement", workspace: "/tmp/w" }),
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

      // Prune the issue (worker is dead in map + has crash history)
      const pruneResponse = await requestJson("/workers/prune", {
        method: "POST",
        body: JSON.stringify({ issueIds: ["ENG-300"] }),
      });
      expect(pruneResponse.status).toBe(200);
      const pruneBody = (await pruneResponse.json()) as {
        pruned: string[];
        crashHistoryPruned: string[];
      };
      expect(pruneBody.pruned).toEqual(["eng-300-implement"]);
      expect(pruneBody.crashHistoryPruned).toEqual(["eng-300-implement"]);

      // Verify crash history is gone — respawn succeeds despite previous 3 crashes
      const respawn = await requestJson("/workers", {
        method: "POST",
        body: JSON.stringify({ issueId: "ENG-300", mode: "implement", workspace: "/tmp/w" }),
      });
      expect(respawn.status).toBe(200);
    });
    it("returns empty lists when no workers match", async () => {
      await startTestServer();
      await requestJson("/workers", {
        method: "POST",
        body: JSON.stringify({ issueId: "ENG-400", mode: "implement", workspace: "/tmp/w" }),
      });

      const pruneResponse = await requestJson("/workers/prune", {
        method: "POST",
        body: JSON.stringify({ issueIds: ["ENG-999"] }),
      });
      expect(pruneResponse.status).toBe(200);
      const body = (await pruneResponse.json()) as {
        pruned: string[];
        crashHistoryPruned: string[];
      };
      expect(body.pruned).toEqual([]);
      expect(body.crashHistoryPruned).toEqual([]);

      // Original worker still exists
      const listResponse = await requestJson("/workers");
      const workers = (await listResponse.json()) as WorkerEntry[];
      expect(workers).toHaveLength(1);
    });

    it("rejects missing issueIds field", async () => {
      await startTestServer();
      const response = await requestJson("/workers/prune", {
        method: "POST",
        body: JSON.stringify({}),
      });
      expect(response.status).toBe(400);
    });

    it("rejects non-array issueIds", async () => {
      await startTestServer();
      const response = await requestJson("/workers/prune", {
        method: "POST",
        body: JSON.stringify({ issueIds: "not-an-array" }),
      });
      expect(response.status).toBe(400);
    });

    it("handles case-insensitive issue ID matching", async () => {
      await startTestServer();
      await requestJson("/workers", {
        method: "POST",
        body: JSON.stringify({ issueId: "ENG-500", mode: "implement", workspace: "/tmp/w" }),
      });

      // Prune with different casing
      const pruneResponse = await requestJson("/workers/prune", {
        method: "POST",
        body: JSON.stringify({ issueIds: ["eng-500"] }),
      });
      expect(pruneResponse.status).toBe(200);
      const body = (await pruneResponse.json()) as { pruned: string[] };
      expect(body.pruned).toEqual(["eng-500-implement"]);
    });

    it("persists state after pruning", async () => {
      await startTestServer();
      await requestJson("/workers", {
        method: "POST",
        body: JSON.stringify({ issueId: "ENG-600", mode: "implement", workspace: "/tmp/w" }),
      });

      await requestJson("/workers/prune", {
        method: "POST",
        body: JSON.stringify({ issueIds: ["ENG-600"] }),
      });

      // Read state file directly to verify persistence
      const raw = await readFile(path.join(tempDir ?? os.tmpdir(), "workers.json"), "utf-8");
      const parsed = JSON.parse(raw) as PersistedWorkerState;
      expect(Object.keys(parsed.workers)).toHaveLength(0);
    });

    it("prunes workers loaded from pre-existing state file", async () => {
      const existing: WorkerEntry = {
        id: "eng-700-implement",
        port: sharedServePort,
        sessionId: computeSessionId(legionId, "eng-700", "implement"),
        workspace: "/tmp",
        startedAt: "2026-02-01T00:00:00.000Z",
        status: "running",
        crashCount: 0,
        lastCrashAt: null,
      };
      await startTestServer({
        state: {
          workers: { [existing.id]: existing },
          crashHistory: {
            "eng-700-implement": { crashCount: 1, lastCrashAt: "2026-01-01T00:00:00.000Z" },
          },
        },
      });

      const pruneResponse = await requestJson("/workers/prune", {
        method: "POST",
        body: JSON.stringify({ issueIds: ["eng-700"] }),
      });
      expect(pruneResponse.status).toBe(200);
      const body = (await pruneResponse.json()) as {
        pruned: string[];
        crashHistoryPruned: string[];
      };
      expect(body.pruned).toEqual(["eng-700-implement"]);
      expect(body.crashHistoryPruned).toEqual(["eng-700-implement"]);

      // Verify workers list is empty
      const listResponse = await requestJson("/workers");
      const workers = (await listResponse.json()) as WorkerEntry[];
      expect(workers).toHaveLength(0);
    });

    it("returns 404 for non-POST methods", async () => {
      await startTestServer();
      const response = await requestJson("/workers/prune");
      expect(response.status).toBe(404);
    });
  });

  describe("DELETE /workers/:id/workspace prunes worker", () => {
    it("removes worker entry after workspace cleanup", async () => {
      const paths: LegionPaths = {
        dataDir: "/tmp/legion-data",
        stateDir: "/tmp/legion-state",
        reposDir: "/tmp/legion-data/repos",
        workspacesDir: "/tmp/legion-data/workspaces",
        legionsFile: "/tmp/legion-state/legions.json",
        forLegion: (projectId: string) => ({
          legionStateDir: `/tmp/legion-state/legions/${projectId}`,
          workersFile: `/tmp/legion-state/legions/${projectId}/workers.json`,
          promotedFile: `/tmp/legion-state/legions/${projectId}/promoted.json`,
          feedbackFile: `/tmp/legion-state/legions/${projectId}/feedback.jsonl`,
          logDir: `/tmp/legion-state/legions/${projectId}/logs`,
          workspacesDir: `/tmp/legion-data/workspaces/${projectId}`,
        }),
        repoClonePath: (host: string, owner: string, repo: string) =>
          `/tmp/legion-data/repos/${host}/${owner}/${repo}`,
      };
      const repoManagerDeps: RepoManagerDeps = {
        runJj: async () => ({ exitCode: 0, stdout: "", stderr: "" }),
        exists: async () => true,
        rmDir: async () => {},
        symlink: async () => {},
      };
      await startTestServer({ paths, repoManagerDeps });

      // Create a worker with repo context
      const createRes = await requestJson("/workers", {
        method: "POST",
        body: JSON.stringify({
          issueId: "acme-widgets-800",
          mode: "implement",
          repo: "acme/widgets",
          issueNumber: 800,
        }),
      });
      expect(createRes.status).toBe(200);
      const created = (await createRes.json()) as { id: string };

      // Verify worker exists
      const before = await requestJson("/workers");
      const workersBefore = (await before.json()) as WorkerEntry[];
      expect(workersBefore).toHaveLength(1);

      // Delete workspace — should also remove the worker
      const deleteRes = await requestJson(`/workers/${created.id}/workspace`, {
        method: "DELETE",
        body: JSON.stringify({ repo: "acme/widgets" }),
      });
      expect(deleteRes.status).toBe(200);
      const deleteBody = (await deleteRes.json()) as { status: string; workerRemoved: boolean };
      expect(deleteBody.workerRemoved).toBe(true);

      // Verify worker is gone
      const after = await requestJson("/workers");
      const workersAfter = (await after.json()) as WorkerEntry[];
      expect(workersAfter).toHaveLength(0);
    });
  });

  describe("GET /dashboard", () => {
    it("returns empty dashboard when no workers exist", async () => {
      await startTestServer();
      const response = await requestJson("/dashboard");
      expect(response.status).toBe(200);
      const body = (await response.json()) as {
        generatedAt: string;
        summary: {
          totalWorkers: number;
          byStatus: Record<string, number>;
          byPhase: Record<string, number>;
        };
        groups: Record<string, unknown>;
        recentEvents: unknown[];
      };
      expect(body.generatedAt).toBeTruthy();
      expect(body.summary.totalWorkers).toBe(0);
      expect(body.summary.byStatus).toEqual({});
      expect(body.summary.byPhase).toEqual({});
      expect(body.groups).toEqual({});
      expect(body.recentEvents).toEqual([]);
    });

    it("groups workers by repo and issue number with summary stats", async () => {
      sessionStatusHandler = async () => ({
        data: {
          type: "busy",
          lastActivityAt: "2026-04-09T10:00:00Z",
          messageCount: 20,
          turnCount: 15,
          phase: "busy",
          tokensUsed: 50000,
        },
      });
      const state: PersistedWorkerState = {
        workers: {
          "acme-widgets-10-implement": {
            id: "acme-widgets-10-implement",
            port: sharedServePort,
            sessionId: "ses_w1",
            workspace: "/tmp/w1",
            startedAt: "2026-04-09T00:00:00Z",
            status: "running",
            crashCount: 0,
            lastCrashAt: null,
            repo: "acme/widgets",
            issueNumber: 10,
          },
          "acme-widgets-10-test": {
            id: "acme-widgets-10-test",
            port: sharedServePort,
            sessionId: "ses_w2",
            workspace: "/tmp/w2",
            startedAt: "2026-04-09T00:01:00Z",
            status: "running",
            crashCount: 0,
            lastCrashAt: null,
            repo: "acme/widgets",
            issueNumber: 10,
          },
          "acme-api-5-review": {
            id: "acme-api-5-review",
            port: sharedServePort,
            sessionId: "ses_w3",
            workspace: "/tmp/w3",
            startedAt: "2026-04-09T00:02:00Z",
            status: "running",
            crashCount: 0,
            lastCrashAt: null,
            repo: "acme/api",
            issueNumber: 5,
          },
        },
        crashHistory: {},
      };
      await startTestServer({ state });

      const response = await requestJson("/dashboard");
      expect(response.status).toBe(200);
      const body = (await response.json()) as {
        generatedAt: string;
        summary: {
          totalWorkers: number;
          byStatus: Record<string, number>;
          byPhase: Record<string, number>;
        };
        groups: Record<
          string,
          Record<
            string,
            {
              issueTitle: string | null;
              issueStatus: string | null;
              workers: Array<{ id: string; phase: string; status: string; activity: unknown }>;
            }
          >
        >;
        recentEvents: Array<{ event: string; workerId: string }>;
      };

      // Summary stats
      expect(body.summary.totalWorkers).toBe(3);
      expect(body.summary.byStatus).toEqual({ running: 3 });
      expect(body.summary.byPhase).toEqual({ implement: 1, test: 1, review: 1 });

      // Grouping by repo
      expect(Object.keys(body.groups)).toEqual(
        expect.arrayContaining(["acme/widgets", "acme/api"])
      );

      // acme/widgets has issue 10 with 2 workers
      const widgetsIssue10 = body.groups["acme/widgets"]["10"];
      expect(widgetsIssue10).toBeDefined();
      expect(widgetsIssue10.workers).toHaveLength(2);
      const phases = widgetsIssue10.workers.map((w: { phase: string }) => w.phase).sort();
      expect(phases).toEqual(["implement", "test"]);

      // acme/api has issue 5 with 1 worker
      const apiIssue5 = body.groups["acme/api"]["5"];
      expect(apiIssue5).toBeDefined();
      expect(apiIssue5.workers).toHaveLength(1);
      expect(apiIssue5.workers[0].phase).toBe("review");

      // Activity is populated
      expect(widgetsIssue10.workers[0].activity).toEqual({
        type: "busy",
        messageCount: 20,
        turnCount: 15,
        tokensUsed: 50000,
        lastActivityAt: "2026-04-09T10:00:00Z",
      });
    });

    it("handles activity fetch failures gracefully", async () => {
      sessionStatusHandler = async () => {
        throw new Error("connection refused");
      };
      await startTestServer();

      await requestJson("/workers", {
        method: "POST",
        body: JSON.stringify({
          issueId: "ENG-99",
          mode: "implement",
          workspace: "/tmp/w-fail",
        }),
      });

      const response = await requestJson("/dashboard");
      expect(response.status).toBe(200);
      const body = (await response.json()) as {
        groups: Record<string, Record<string, { workers: Array<{ activity: unknown }> }>>;
      };

      // Worker should still appear but with null activity
      const unknownGroup = body.groups._unknown;
      expect(unknownGroup).toBeDefined();
      const workers = Object.values(unknownGroup)[0].workers;
      expect(workers).toHaveLength(1);
      expect(workers[0].activity).toBeNull();
    });

    it("records status change events in recentEvents", async () => {
      await startTestServer();

      // Create a worker
      const createRes = await requestJson("/workers", {
        method: "POST",
        body: JSON.stringify({
          issueId: "ENG-55",
          mode: "plan",
          workspace: "/tmp/w-event",
        }),
      });
      const created = (await createRes.json()) as { id: string };

      // Change its status to dead
      await requestJson(`/workers/${created.id}`, {
        method: "PATCH",
        body: JSON.stringify({
          status: "dead",
          crashCount: 1,
          lastCrashAt: "2026-04-09T00:00:00Z",
        }),
      });

      const response = await requestJson("/dashboard");
      const body = (await response.json()) as {
        recentEvents: Array<{
          event: string;
          workerId: string;
          mode: string;
          details?: Record<string, unknown>;
        }>;
      };

      // Should have 2 events: dispatch + status_changed (newest first)
      expect(body.recentEvents).toHaveLength(2);
      expect(body.recentEvents[0].event).toBe("worker.status_changed");
      expect(body.recentEvents[0].workerId).toBe(created.id);
      expect(body.recentEvents[0].details?.toStatus).toBe("dead");
      expect(body.recentEvents[1].event).toBe("worker.dispatched");
    });

    it("populates issue title cache from state collection", async () => {
      sessionStatusHandler = async () => ({ data: { type: "idle" } });
      const state: PersistedWorkerState = {
        workers: {
          "acme-widgets-42-implement": {
            id: "acme-widgets-42-implement",
            port: sharedServePort,
            sessionId: "ses_title",
            workspace: "/tmp/w-title",
            startedAt: "2026-04-09T00:00:00Z",
            status: "running",
            crashCount: 0,
            lastCrashAt: null,
            repo: "acme/widgets",
            issueNumber: 42,
          },
        },
        crashHistory: {},
      };
      await startTestServer({ state });

      // Mock fetch for enrichParsedIssues (needs GET /workers + GraphQL)
      const mockFn = async (input: string | URL | Request, init?: RequestInit) => {
        const urlStr =
          typeof input === "string" ? input : input instanceof URL ? input.toString() : input.url;
        if (
          urlStr.includes("/workers") &&
          (!init || init.method === undefined || init.method === "GET")
        ) {
          return new Response(
            JSON.stringify([
              {
                id: "acme-widgets-42-implement",
                port: sharedServePort,
                sessionId: "ses_title",
                workspace: "/tmp/w-title",
                startedAt: new Date().toISOString(),
                status: "running",
                crashCount: 0,
                lastCrashAt: null,
                repo: "acme/widgets",
                issueNumber: 42,
              },
            ]),
            { status: 200, headers: { "content-type": "application/json" } }
          );
        }
        if (urlStr.includes("graphql")) {
          return new Response(JSON.stringify({ data: {} }), {
            status: 200,
            headers: { "content-type": "application/json" },
          });
        }
        return originalFetch(input as string | URL | Request, init);
      };
      globalThis.fetch = Object.assign(mockFn, {
        preconnect: originalFetch.preconnect,
      });

      await requestJson("/state/collect", {
        method: "POST",
        body: JSON.stringify({
          backend: "github",
          issues: {
            items: [
              {
                content: {
                  type: "Issue",
                  number: 42,
                  repository: "acme/widgets",
                  url: "https://github.com/acme/widgets/issues/42",
                  title: "Fix the widget alignment",
                },
                status: "In Progress",
                labels: [],
              },
            ],
          },
        }),
      });

      // Dashboard should now have the cached title
      const response = await requestJson("/dashboard");
      expect(response.status).toBe(200);
      const body = (await response.json()) as {
        groups: Record<
          string,
          Record<string, { issueTitle: string | null; issueStatus: string | null }>
        >;
      };

      const widgets = body.groups["acme/widgets"];
      expect(widgets).toBeDefined();
      expect(widgets["42"].issueTitle).toBe("Fix the widget alignment");
      expect(widgets["42"].issueStatus).toBe("In Progress");
    });

    it("loads workers from persisted state", async () => {
      sessionStatusHandler = async () => ({ data: { type: "idle" } });
      const state: PersistedWorkerState = {
        workers: {
          "acme-widgets-20-implement": {
            id: "acme-widgets-20-implement",
            port: sharedServePort,
            sessionId: "ses_persisted1",
            workspace: "/tmp/persisted",
            startedAt: "2026-04-09T00:00:00Z",
            status: "running",
            crashCount: 0,
            lastCrashAt: null,
            repo: "acme/widgets",
            issueNumber: 20,
          },
        },
        crashHistory: {},
      };
      await startTestServer({ state });

      const response = await requestJson("/dashboard");
      expect(response.status).toBe(200);
      const body = (await response.json()) as {
        summary: { totalWorkers: number };
        groups: Record<string, Record<string, { workers: Array<{ id: string }> }>>;
      };
      expect(body.summary.totalWorkers).toBe(1);
      expect(body.groups["acme/widgets"]["20"].workers[0].id).toBe("acme-widgets-20-implement");
    });
  });

  describe("GET /dashboard/ui", () => {
    it("returns HTML page with correct content-type", async () => {
      await startTestServer();
      const response = await originalFetch(`${baseUrl}/dashboard/ui`);
      expect(response.status).toBe(200);
      expect(response.headers.get("content-type")).toBe("text/html; charset=utf-8");
      const html = await response.text();
      expect(html).toContain("<!DOCTYPE html>");
      expect(html).toContain("Legion Dashboard");
      expect(html).toContain('fetch("/dashboard")');
    });

    it("includes auto-refresh and responsive meta tag", async () => {
      await startTestServer();
      const response = await originalFetch(`${baseUrl}/dashboard/ui`);
      const html = await response.text();
      expect(html).toContain('name="viewport"');
      expect(html).toContain("REFRESH_INTERVAL = 30");
    });
  });

  describe("GET /state/track", () => {
    it("returns empty list when no issues tracked", async () => {
      await startTestServer({});
      const response = await requestJson("/state/track");
      expect(response.status).toBe(200);
      const body = (await response.json()) as { trackedIssues: string[] };
      expect(body.trackedIssues).toEqual([]);
    });

    it("returns sorted list of tracked issues", async () => {
      await startTestServer({});
      await requestJson("/state/track", {
        method: "POST",
        body: JSON.stringify({ issueId: "issue-b" }),
      });
      await requestJson("/state/track", {
        method: "POST",
        body: JSON.stringify({ issueId: "issue-a" }),
      });
      const response = await requestJson("/state/track");
      expect(response.status).toBe(200);
      const body = (await response.json()) as { trackedIssues: string[] };
      expect(body.trackedIssues).toEqual(["issue-a", "issue-b"]);
    });
  });

  describe("POST /state/track", () => {
    it("tracks an issue and returns tracked: true", async () => {
      await startTestServer({});
      const response = await requestJson("/state/track", {
        method: "POST",
        body: JSON.stringify({ issueId: "my-issue-1" }),
      });
      expect(response.status).toBe(200);
      const body = (await response.json()) as { tracked: boolean };
      expect(body.tracked).toBe(true);
    });

    it("normalizes issueId to lowercase", async () => {
      await startTestServer({});
      await requestJson("/state/track", {
        method: "POST",
        body: JSON.stringify({ issueId: "MY-ISSUE-1" }),
      });
      const listResponse = await requestJson("/state/track");
      const body = (await listResponse.json()) as { trackedIssues: string[] };
      expect(body.trackedIssues).toContain("my-issue-1");
    });

    it("is idempotent — tracking same issue twice does not duplicate", async () => {
      await startTestServer({});
      await requestJson("/state/track", {
        method: "POST",
        body: JSON.stringify({ issueId: "issue-1" }),
      });
      await requestJson("/state/track", {
        method: "POST",
        body: JSON.stringify({ issueId: "issue-1" }),
      });
      const listResponse = await requestJson("/state/track");
      const body = (await listResponse.json()) as { trackedIssues: string[] };
      expect(body.trackedIssues.filter((id) => id === "issue-1")).toHaveLength(1);
    });

    it("returns 400 when issueId is missing", async () => {
      await startTestServer({});
      const response = await requestJson("/state/track", {
        method: "POST",
        body: JSON.stringify({}),
      });
      expect(response.status).toBe(400);
    });

    it("returns 400 when issueId is not a string", async () => {
      await startTestServer({});
      const response = await requestJson("/state/track", {
        method: "POST",
        body: JSON.stringify({ issueId: 42 }),
      });
      expect(response.status).toBe(400);
    });
  });

  describe("DELETE /state/track/:issueId", () => {
    it("untracks an issue and returns untracked: true", async () => {
      await startTestServer({});
      await requestJson("/state/track", {
        method: "POST",
        body: JSON.stringify({ issueId: "issue-1" }),
      });
      const response = await requestJson("/state/track/issue-1", { method: "DELETE" });
      expect(response.status).toBe(200);
      const body = (await response.json()) as { untracked: boolean };
      expect(body.untracked).toBe(true);

      const listResponse = await requestJson("/state/track");
      const listBody = (await listResponse.json()) as { trackedIssues: string[] };
      expect(listBody.trackedIssues).not.toContain("issue-1");
    });

    it("is idempotent — untracking a non-tracked issue returns untracked: true", async () => {
      await startTestServer({});
      const response = await requestJson("/state/track/nonexistent-issue", { method: "DELETE" });
      expect(response.status).toBe(200);
      const body = (await response.json()) as { untracked: boolean };
      expect(body.untracked).toBe(true);
    });

    it("normalizes issueId to lowercase", async () => {
      await startTestServer({});
      await requestJson("/state/track", {
        method: "POST",
        body: JSON.stringify({ issueId: "issue-1" }),
      });
      const response = await requestJson("/state/track/ISSUE-1", { method: "DELETE" });
      expect(response.status).toBe(200);
      const listResponse = await requestJson("/state/track");
      const body = (await listResponse.json()) as { trackedIssues: string[] };
      expect(body.trackedIssues).not.toContain("issue-1");
    });
  });

  describe("auto-track on POST /workers", () => {
    it("auto-tracks issue when worker is dispatched", async () => {
      await startTestServer({});
      await requestJson("/workers", {
        method: "POST",
        body: JSON.stringify({
          issueId: "ENG-42",
          mode: "implement",
          workspace: "/tmp/workspace",
        }),
      });
      const response = await requestJson("/state/track");
      const body = (await response.json()) as { trackedIssues: string[] };
      expect(body.trackedIssues).toContain("eng-42");
    });
  });

  describe("auto-untrack on cleanupDoneIssueWorkers", () => {
    it("auto-untracks issue when it is cleaned up as Done", async () => {
      await startTestServer({});

      // Track the issue
      await requestJson("/state/track", {
        method: "POST",
        body: JSON.stringify({ issueId: "acme-widgets-42" }),
      });

      // Verify it's tracked
      const beforeResponse = await requestJson("/state/track");
      const beforeBody = (await beforeResponse.json()) as { trackedIssues: string[] };
      expect(beforeBody.trackedIssues).toContain("acme-widgets-42");

      // Collect state with issue as Done — triggers cleanup + untrack
      await requestJson("/state/collect", {
        method: "POST",
        body: JSON.stringify({
          backend: "github",
          issues: [createGitHubProjectItem({ status: "Done" })],
        }),
      });
      await Bun.sleep(100);

      const afterResponse = await requestJson("/state/track");
      const afterBody = (await afterResponse.json()) as { trackedIssues: string[] };
      expect(afterBody.trackedIssues).not.toContain("acme-widgets-42");
    });
  });

  describe("GET /state/materialized", () => {
    it("returns empty issues and titles when nothing tracked", async () => {
      await startTestServer({});
      const response = await requestJson("/state/materialized");
      expect(response.status).toBe(200);
      const body = (await response.json()) as {
        issues: Record<string, unknown>;
        titles: Record<string, string>;
        newIssues: unknown[];
      };
      expect(body.issues).toEqual({});
      expect(body.titles).toEqual({});
      expect(body.newIssues).toEqual([]);
    });

    it("returns tracked issues from cache", async () => {
      await startTestServer({});

      // Track and collect state
      await requestJson("/state/track", {
        method: "POST",
        body: JSON.stringify({ issueId: "acme-widgets-42" }),
      });
      await requestJson("/state/collect", {
        method: "POST",
        body: JSON.stringify({
          backend: "github",
          issues: [createGitHubProjectItem({ status: "In Progress" })],
        }),
      });

      const response = await requestJson("/state/materialized");
      expect(response.status).toBe(200);
      const body = (await response.json()) as {
        issues: Record<string, { status: string }>;
        titles: Record<string, string>;
        newIssues: unknown[];
      };
      expect(body.issues["acme-widgets-42"]).toBeDefined();
      expect(body.issues["acme-widgets-42"].status).toBe("In Progress");
    });

    it("does not return untracked issues", async () => {
      await startTestServer({});

      // Collect state without tracking
      await requestJson("/state/collect", {
        method: "POST",
        body: JSON.stringify({
          backend: "github",
          issues: [createGitHubProjectItem({ status: "In Progress" })],
        }),
      });

      const response = await requestJson("/state/materialized");
      const body = (await response.json()) as { issues: Record<string, unknown> };
      expect(Object.keys(body.issues)).toHaveLength(0);
    });

    it("drains newIssues accumulator on read", async () => {
      await startTestServer({
        getControllerState: () => ({ sessionId: "ses_ctrl" }),
      });

      // First collect establishes baseline
      await requestJson("/state/collect", {
        method: "POST",
        body: JSON.stringify({
          backend: "github",
          issues: [createGitHubProjectItem({ number: 10, status: "Todo" })],
        }),
      });

      // Track issue 11 so it flows through the trackedIssueIds filter
      await requestJson("/state/track", {
        method: "POST",
        body: JSON.stringify({ issueId: "acme-widgets-11" }),
      });

      // Second collect adds a new issue
      await requestJson("/state/collect", {
        method: "POST",
        body: JSON.stringify({
          backend: "github",
          issues: [
            createGitHubProjectItem({ number: 10, status: "Todo" }),
            createGitHubProjectItem({ number: 11, status: "Todo" }),
          ],
        }),
      });
      await Bun.sleep(50);

      // First read should have the new issue
      const firstRead = await requestJson("/state/materialized");
      const firstBody = (await firstRead.json()) as {
        newIssues: Array<{ issueId: string }>;
      };
      expect(firstBody.newIssues.some((e) => e.issueId === "acme-widgets-11")).toBe(true);

      // Second read should have empty newIssues (drained)
      const secondRead = await requestJson("/state/materialized");
      const secondBody = (await secondRead.json()) as { newIssues: unknown[] };
      expect(secondBody.newIssues).toHaveLength(0);
    });
  });
});

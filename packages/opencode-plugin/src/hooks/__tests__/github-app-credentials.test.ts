import { afterEach, beforeEach, describe, expect, it, mock } from "bun:test";
import { createGitHubAppCredentialsHook } from "../github-app-credentials";

interface WorkerRecord {
  id: string;
  sessionId: string;
  workspace: string;
}

interface TokenResponse {
  role: string;
  owner: string;
  expiresAt: string;
  env: Record<string, string>;
}

const ORIGINAL_DAEMON_PORT = process.env.LEGION_DAEMON_PORT;

function jsonResponse(data: unknown, status = 200): Response {
  return new Response(JSON.stringify(data), {
    status,
    headers: { "content-type": "application/json" },
  });
}

function makeHookContext(port?: string) {
  return {
    input: {
      cwd: "/tmp/workspaces/worker",
      sessionID: "session-1",
    },
    output: {
      env: port ? { LEGION_DAEMON_PORT: port } : ({} as Record<string, string>),
    },
  };
}

function makeFetchFn(options: {
  workers: WorkerRecord[];
  tokenByWorkerId?: Record<string, TokenResponse>;
  workerStatus?: number;
  tokenStatusByWorkerId?: Record<string, number>;
  throwOnWorkers?: boolean;
}) {
  const calls: string[] = [];
  const fetchFn = mock(async (url: string | URL | Request): Promise<Response> => {
    const urlString = typeof url === "string" ? url : url.toString();
    calls.push(urlString);

    if (urlString.endsWith("/workers")) {
      if (options.throwOnWorkers) {
        throw new Error("daemon unreachable");
      }
      return jsonResponse(options.workers, options.workerStatus ?? 200);
    }

    const workerMatch = urlString.match(/\/workers\/([^/]+)\/token$/);
    if (!workerMatch) {
      throw new Error(`unexpected url: ${urlString}`);
    }

    const workerId = decodeURIComponent(workerMatch[1] ?? "");
    const status = options.tokenStatusByWorkerId?.[workerId] ?? 200;
    if (status !== 200) {
      return jsonResponse({ error: "token unavailable" }, status);
    }

    const token = options.tokenByWorkerId?.[workerId];
    if (!token) {
      throw new Error(`missing token for worker ${workerId}`);
    }

    return jsonResponse(token, 200);
  });

  return { fetchFn: fetchFn as unknown as typeof fetch, calls };
}

describe("createGitHubAppCredentialsHook", () => {
  beforeEach(() => {
    delete process.env.LEGION_DAEMON_PORT;
  });

  afterEach(() => {
    if (ORIGINAL_DAEMON_PORT === undefined) {
      delete process.env.LEGION_DAEMON_PORT;
      return;
    }

    process.env.LEGION_DAEMON_PORT = ORIGINAL_DAEMON_PORT;
  });

  it("injects credentials for matching sessionID", async () => {
    const { input, output } = makeHookContext("13381");
    const { fetchFn, calls } = makeFetchFn({
      workers: [{ id: "worker-session", sessionId: "session-1", workspace: "/tmp/workspaces/one" }],
      tokenByWorkerId: {
        "worker-session": {
          role: "github-app",
          owner: "trajectory-labs",
          expiresAt: new Date("2030-01-01T01:00:00.000Z").toISOString(),
          env: {
            GH_TOKEN: "token-session",
            GIT_AUTHOR_NAME: "Legion Worker",
          },
        },
      },
    });
    const hook = createGitHubAppCredentialsHook({ fetchFn, now: () => 0 });

    await hook(input, output);

    expect(output.env.GH_TOKEN).toBe("token-session");
    expect(output.env.GIT_AUTHOR_NAME).toBe("Legion Worker");
    expect(calls).toEqual([
      "http://127.0.0.1:13381/workers",
      "http://127.0.0.1:13381/workers/worker-session/token",
    ]);
  });

  it("falls back to cwd lookup when sessionID is missing", async () => {
    const { output } = makeHookContext("14444");
    const input = { cwd: "/tmp/workspaces/matching/nested/project" };
    const { fetchFn } = makeFetchFn({
      workers: [
        { id: "worker-cwd", sessionId: "different-session", workspace: "/tmp/workspaces/matching" },
      ],
      tokenByWorkerId: {
        "worker-cwd": {
          role: "github-app",
          owner: "trajectory-labs",
          expiresAt: new Date("2030-01-01T01:00:00.000Z").toISOString(),
          env: {
            GH_TOKEN: "token-cwd",
          },
        },
      },
    });
    const hook = createGitHubAppCredentialsHook({ fetchFn, now: () => 0 });

    await hook(input, output);

    expect(output.env.GH_TOKEN).toBe("token-cwd");
  });

  it("reuses cached credentials until the refresh window", async () => {
    const { input, output } = makeHookContext("15555");
    const { fetchFn, calls } = makeFetchFn({
      workers: [{ id: "worker-cache", sessionId: "session-1", workspace: "/tmp/workspaces/cache" }],
      tokenByWorkerId: {
        "worker-cache": {
          role: "github-app",
          owner: "trajectory-labs",
          expiresAt: new Date("2030-01-01T01:00:00.000Z").toISOString(),
          env: {
            GH_TOKEN: "token-cache",
          },
        },
      },
    });
    let currentTime = 0;
    const hook = createGitHubAppCredentialsHook({
      fetchFn: fetchFn as typeof fetch,
      now: () => currentTime,
    });

    await hook(input, output);
    output.env = { LEGION_DAEMON_PORT: "15555" };
    currentTime = 60_000;

    await hook(input, output);

    expect(output.env.GH_TOKEN).toBe("token-cache");
    expect(calls.filter((url) => url.endsWith("/workers/worker-cache/token"))).toHaveLength(1);
  });

  it("refreshes credentials near expiry", async () => {
    const { input, output } = makeHookContext("16666");
    let tokenFetches = 0;
    const fetchFn = mock(async (url: string | URL | Request): Promise<Response> => {
      const urlString = typeof url === "string" ? url : url.toString();
      if (urlString.endsWith("/workers")) {
        return jsonResponse([
          { id: "worker-refresh", sessionId: "session-1", workspace: "/tmp/workspaces/refresh" },
        ]);
      }

      tokenFetches += 1;
      return jsonResponse({
        role: "github-app",
        owner: "trajectory-labs",
        expiresAt: new Date(10 * 60 * 1000).toISOString(),
        env: {
          GH_TOKEN: `token-refresh-${tokenFetches}`,
        },
      });
    });
    let currentTime = 0;
    const hook = createGitHubAppCredentialsHook({
      fetchFn: fetchFn as unknown as typeof fetch,
      now: () => currentTime,
    });

    await hook(input, output);
    output.env = { LEGION_DAEMON_PORT: "16666" };
    currentTime = 6 * 60 * 1000;

    await hook(input, output);

    expect(output.env.GH_TOKEN).toBe("token-refresh-2");
    expect(tokenFetches).toBe(2);
  });

  it("no-ops when LEGION_DAEMON_PORT is unavailable", async () => {
    const { input, output } = makeHookContext();
    const fetchFn = mock(
      async (): Promise<Response> => jsonResponse([])
    ) as unknown as typeof fetch;
    const hook = createGitHubAppCredentialsHook({ fetchFn, now: () => 0 });

    await hook(input, output);

    expect(output.env).toEqual({});
    expect(fetchFn).not.toHaveBeenCalled();
  });

  it("no-ops when the daemon is unreachable", async () => {
    const { input, output } = makeHookContext("17777");
    const { fetchFn } = makeFetchFn({
      workers: [],
      throwOnWorkers: true,
    });
    const hook = createGitHubAppCredentialsHook({ fetchFn, now: () => 0 });

    await hook(input, output);

    expect(output.env).toEqual({ LEGION_DAEMON_PORT: "17777" });
  });

  it("no-ops when no worker matches the session or cwd", async () => {
    const { output } = makeHookContext("18888");
    const input = {
      cwd: "/tmp/workspaces/no-match",
      sessionID: "missing-session",
    };
    const { fetchFn, calls } = makeFetchFn({
      workers: [
        { id: "worker-other", sessionId: "other-session", workspace: "/tmp/workspaces/other" },
      ],
    });
    const hook = createGitHubAppCredentialsHook({ fetchFn, now: () => 0 });

    await hook(input, output);

    expect(output.env).toEqual({ LEGION_DAEMON_PORT: "18888" });
    expect(calls).toEqual(["http://127.0.0.1:18888/workers"]);
  });

  it("no-ops when the token endpoint returns an error", async () => {
    const { input, output } = makeHookContext("19999");
    const { fetchFn } = makeFetchFn({
      workers: [
        { id: "worker-missing-token", sessionId: "session-1", workspace: "/tmp/workspaces/token" },
      ],
      tokenStatusByWorkerId: {
        "worker-missing-token": 404,
      },
    });
    const hook = createGitHubAppCredentialsHook({ fetchFn, now: () => 0 });

    await hook(input, output);

    expect(output.env).toEqual({ LEGION_DAEMON_PORT: "19999" });
  });
});

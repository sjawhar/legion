import { describe, expect, it, mock } from "bun:test";
import type { GitHubAppsConfig } from "../config";
import { TokenManager } from "../github-apps";
import { RoleServeManager } from "../multi-serve";
import type { RuntimeAdapter, RuntimeStartOptions } from "../runtime/types";

function createMockAdapter(port: number): RuntimeAdapter & {
  startCalls: RuntimeStartOptions[];
  sessions: Map<string, string>;
  _healthy: boolean;
} {
  const startCalls: RuntimeStartOptions[] = [];
  const sessions = new Map<string, string>();
  const _healthy = true;

  return {
    startCalls,
    sessions,
    _healthy,
    async start(opts: RuntimeStartOptions) {
      startCalls.push(opts);
    },
    async stop() {},
    async healthy() {
      return _healthy;
    },
    async createSession(sessionId: string, workspace: string) {
      sessions.set(sessionId, workspace);
      return sessionId;
    },
    async sendPrompt() {},
    getPort() {
      return port;
    },
    async getSessionStatus() {
      return { data: { type: "idle" } };
    },
  };
}

function createTestKeyPem(): string {
  // Minimal valid PEM for test — we won't actually sign JWTs in these tests
  return "-----BEGIN PRIVATE KEY-----\nfake\n-----END PRIVATE KEY-----";
}

function createTestConfig(): GitHubAppsConfig {
  return {
    impl: {
      appId: "111",
      privateKeyPath: "/tmp/impl.pem",
      installationId: "222",
    },
    review: {
      appId: "333",
      privateKeyPath: "/tmp/review.pem",
      installationId: "444",
    },
  };
}

function createTestTokenManager(config: GitHubAppsConfig): TokenManager {
  const fetchFn = mock(async () => {
    const expiresAt = new Date(Date.now() + 60 * 60 * 1000).toISOString();
    return new Response(JSON.stringify({ token: "ghs_test_token", expires_at: expiresAt }), {
      status: 201,
      headers: { "content-type": "application/json" },
    });
  }) as unknown as typeof fetch;

  // Generate a real key pair synchronously for tests
  const readFile = mock(async () => {
    // We need a real key for TokenManager — but since we mock fetch,
    // generateJwt will be called with this key. Let's use a pre-generated one.
    // Actually, for unit tests of multi-serve, we can mock at a higher level.
    // Let's create a TokenManager with a fake key that will work with mocked fetch.
    return createTestKeyPem();
  });

  return new TokenManager(config, { fetchFn, readFile });
}

describe("RoleServeManager", () => {
  it("routes modes to correct adapters", async () => {
    const config = createTestConfig();
    const fallback = createMockAdapter(13381);

    // We need a real TokenManager for getConfiguredRoles, but we'll mock
    // the start to avoid actual crypto operations
    const manager = new RoleServeManager({
      githubApps: config,
      tokenManager: createTestTokenManager(config),
      runtime: "opencode",
      basePort: 13382,
      shortId: "test",
      fallbackAdapter: fallback,
    });

    // Before start, all modes should fall back to fallback adapter
    const preStartAdapter = manager.getAdapterForMode("implement");
    expect(preStartAdapter).toBe(fallback);

    expect(manager.hasRoleServes()).toBe(false);
  });

  it("falls back to shared adapter for unconfigured roles", () => {
    const config: GitHubAppsConfig = {
      impl: {
        appId: "111",
        privateKeyPath: "/tmp/impl.pem",
        installationId: "222",
      },
    };
    const fallback = createMockAdapter(13381);

    const manager = new RoleServeManager({
      githubApps: config,
      tokenManager: createTestTokenManager(config),
      runtime: "opencode",
      basePort: 13382,
      shortId: "test",
      fallbackAdapter: fallback,
    });

    // review is not configured, should use fallback
    const adapter = manager.getAdapterForMode("review");
    expect(adapter).toBe(fallback);
  });

  it("getAdapterForRole returns fallback when role not started", () => {
    // Config with only impl — review should fall back
    const config: GitHubAppsConfig = {
      impl: {
        appId: "111",
        privateKeyPath: "/tmp/impl.pem",
        installationId: "222",
      },
    };
    const fallback = createMockAdapter(13381);

    const manager = new RoleServeManager({
      githubApps: config,
      tokenManager: createTestTokenManager(config),
      runtime: "opencode",
      basePort: 13382,
      shortId: "test",
      fallbackAdapter: fallback,
    });

    // review is not in config
    expect(manager.getAdapterForRole("review")).toBe(fallback);
  });

  it("getEntries returns empty before start", () => {
    const config = createTestConfig();
    const fallback = createMockAdapter(13381);

    const manager = new RoleServeManager({
      githubApps: config,
      tokenManager: createTestTokenManager(config),
      runtime: "opencode",
      basePort: 13382,
      shortId: "test",
      fallbackAdapter: fallback,
    });

    expect(manager.getEntries()).toEqual([]);
  });

  it("stop clears all entries", async () => {
    const config = createTestConfig();
    const fallback = createMockAdapter(13381);

    const manager = new RoleServeManager({
      githubApps: config,
      tokenManager: createTestTokenManager(config),
      runtime: "opencode",
      basePort: 13382,
      shortId: "test",
      fallbackAdapter: fallback,
    });

    await manager.stop();
    expect(manager.getEntries()).toEqual([]);
    expect(manager.hasRoleServes()).toBe(false);
  });
});

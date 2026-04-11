import { afterEach, beforeEach, describe, expect, it, mock } from "bun:test";

// Suppress console.error during tests
const originalError = console.error;
beforeEach(() => {
  console.error = mock(() => {});
});
afterEach(() => {
  console.error = originalError;
});

describe("envoy plugin init", () => {
  it("returns immediately without blocking on port resolution or Envoy calls", async () => {
    // Simulate NATS/Envoy being unavailable — plugin init must still complete fast
    const originalEnvoyUrl = process.env.ENVOY_URL;
    process.env.ENVOY_URL = "http://127.0.0.1:59999"; // Non-existent

    try {
      const pluginModule = await import("../index");
      const initPlugin = pluginModule.default;

      const start = performance.now();
      const hooks = await initPlugin({ serverUrl: new URL("http://127.0.0.1:13381") } as never);
      const elapsed = performance.now() - start;

      // Plugin init must complete in under 1 second regardless of NATS state
      expect(elapsed).toBeLessThan(1000);
      expect(hooks.tool).toBeDefined();
      expect(hooks.tool.envoy_subscribe).toBeDefined();
      expect(hooks.tool.envoy_unsubscribe).toBeDefined();
      expect(hooks.tool.envoy_list).toBeDefined();
      expect(hooks.tool.envoy_send).toBeDefined();
      expect(hooks.tool.envoy_publish).toBeDefined();
      expect(hooks.tool.envoy_whoami).toBeDefined();
      expect(hooks.tool.envoy_sessions).toBeDefined();
    } finally {
      process.env.ENVOY_URL = originalEnvoyUrl;
    }
  });

  it("call() includes a timeout to prevent hanging on unresponsive Envoy", async () => {
    // The call function has AbortSignal.timeout — verify it doesn't hang
    // We test this indirectly: a tool call to non-existent Envoy should reject within timeout
    const originalEnvoyUrl = process.env.ENVOY_URL;
    process.env.ENVOY_URL = "http://127.0.0.1:59999";

    try {
      const pluginModule = await import("../index");
      const initPlugin = pluginModule.default;
      const hooks = await initPlugin({ serverUrl: new URL("http://127.0.0.1:13381") } as never);

      const start = performance.now();
      try {
        await hooks.tool.envoy_list.execute({}, {
          sessionID: "ses_test",
          directory: "/tmp",
          metadata: () => {},
        } as never);
      } catch {
        // Expected to fail — Envoy is not running
      }
      const elapsed = performance.now() - start;

      // Should fail fast due to connection refused, not hang indefinitely
      expect(elapsed).toBeLessThan(6000);
    } finally {
      process.env.ENVOY_URL = originalEnvoyUrl;
    }
  });
});

describe("envoy_whoami", () => {
  it("returns session identity when Envoy is unavailable", async () => {
    const originalEnvoyUrl = process.env.ENVOY_URL;
    const originalHostname = process.env.HOSTNAME;
    process.env.ENVOY_URL = "http://127.0.0.1:59999";
    process.env.HOSTNAME = "test-machine";

    try {
      const pluginModule = await import("../index");
      const initPlugin = pluginModule.default;
      const hooks = await initPlugin({
        serverUrl: new URL("http://127.0.0.1:13381"),
      } as never);

      const result = await hooks.tool.envoy_whoami.execute({}, {
        sessionID: "ses_test_whoami",
        directory: "/tmp/test-workspace",
        metadata: mock(() => {}),
      } as never);

      const parsed = JSON.parse(result);
      expect(parsed.session_id).toBe("ses_test_whoami");
      expect(parsed.machine_id).toBe("test-machine");
      expect(parsed.dir).toBe("/tmp/test-workspace");
      expect(parsed).not.toHaveProperty("topics");
      expect(parsed.port === null || typeof parsed.port === "number").toBe(true);
    } finally {
      process.env.ENVOY_URL = originalEnvoyUrl;
      if (originalHostname === undefined) {
        delete process.env.HOSTNAME;
      } else {
        process.env.HOSTNAME = originalHostname;
      }
    }
  });

  it("uses 'unknown' for machine_id when HOSTNAME is not set", async () => {
    const originalEnvoyUrl = process.env.ENVOY_URL;
    const originalHostname = process.env.HOSTNAME;
    process.env.ENVOY_URL = "http://127.0.0.1:59999";
    delete process.env.HOSTNAME;

    try {
      const pluginModule = await import("../index");
      const initPlugin = pluginModule.default;
      const hooks = await initPlugin({
        serverUrl: new URL("http://127.0.0.1:13381"),
      } as never);

      const result = await hooks.tool.envoy_whoami.execute({}, {
        sessionID: "ses_no_hostname",
        directory: "/tmp",
        metadata: mock(() => {}),
      } as never);

      const parsed = JSON.parse(result);
      expect(parsed.machine_id).toBe("unknown");
    } finally {
      process.env.ENVOY_URL = originalEnvoyUrl;
      if (originalHostname === undefined) {
        delete process.env.HOSTNAME;
      } else {
        process.env.HOSTNAME = originalHostname;
      }
    }
  });
});

describe("envoy_sessions", () => {
  it("rejects with error when Envoy is unavailable", async () => {
    const originalEnvoyUrl = process.env.ENVOY_URL;
    process.env.ENVOY_URL = "http://127.0.0.1:59999";

    try {
      const pluginModule = await import("../index");
      const initPlugin = pluginModule.default;
      const hooks = await initPlugin({
        serverUrl: new URL("http://127.0.0.1:13381"),
      } as never);

      await expect(
        hooks.tool.envoy_sessions.execute({}, {
          sessionID: "ses_test",
          directory: "/tmp",
          metadata: mock(() => {}),
        } as never)
      ).rejects.toThrow();
    } finally {
      process.env.ENVOY_URL = originalEnvoyUrl;
    }
  });
});

describe("session title", () => {
  it("includes title in follow-up subscribe after session activation", async () => {
    const originalEnvoyUrl = process.env.ENVOY_URL;
    process.env.ENVOY_URL = "http://127.0.0.1:59999";

    const fetchCalls: { url: string; body?: string }[] = [];
    const originalFetch = globalThis.fetch;
    globalThis.fetch = (async (input: string | URL | Request, init?: RequestInit) => {
      const url = typeof input === "string" ? input : input instanceof URL ? input.href : input.url;
      if (init?.body) {
        fetchCalls.push({ url, body: init.body as string });
      } else {
        fetchCalls.push({ url });
      }
      // Serve API: return session with title
      if (url.includes("/session/ses_title_test")) {
        return new Response(JSON.stringify({ id: "ses_title_test", title: "Test Title" }), {
          status: 200,
          headers: { "Content-Type": "application/json" },
        });
      }
      // Envoy subscribe calls: return success
      if (url.includes("/v1/interests/subscribe")) {
        return new Response(JSON.stringify({ session_id: "ses_title_test", topics: [] }), {
          status: 200,
          headers: { "Content-Type": "application/json" },
        });
      }
      // Port resolution calls
      if (url.includes("/session") && !url.includes("ses_title_test")) {
        return new Response("not found", { status: 404 });
      }
      throw new Error("connection refused");
    }) as typeof fetch;

    try {
      const pluginModule = await import("../index");
      const hooks = await pluginModule.default({
        serverUrl: new URL("http://127.0.0.1:13381/"),
      } as never);

      await hooks.event({
        event: {
          type: "session.status",
          properties: {
            sessionID: "ses_title_test",
            status: { type: "busy" },
          },
        },
      });

      // Allow async title fetch and follow-up subscribe to complete
      await new Promise((r) => setTimeout(r, 500));

      const subscribeCalls = fetchCalls.filter(
        (c) => c.url.includes("/v1/interests/subscribe") && c.body
      );
      const hasTitle = subscribeCalls.some((c) => {
        const body = JSON.parse(c.body as string);
        return body.title === "Test Title";
      });
      expect(hasTitle).toBe(true);
    } finally {
      globalThis.fetch = originalFetch;
      process.env.ENVOY_URL = originalEnvoyUrl;
    }
  });
});

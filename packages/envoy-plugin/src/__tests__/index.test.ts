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

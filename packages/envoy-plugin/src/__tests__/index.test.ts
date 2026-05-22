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
      const pluginModule = await import("../server");
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
      const pluginModule = await import("../server");
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
      const pluginModule = await import("../server");
      const initPlugin = pluginModule.default;
      const hooks = await initPlugin({
        serverUrl: new URL("http://127.0.0.1:13381"),
      } as never);

      const result = await hooks.tool.envoy_whoami.execute({}, {
        sessionID: "ses_test_whoami",
        directory: "/tmp/test-workspace",
        metadata: mock(() => {}),
      } as never);

      const parsed = JSON.parse(typeof result === "string" ? result : result.output);
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
      const pluginModule = await import("../server");
      const initPlugin = pluginModule.default;
      const hooks = await initPlugin({
        serverUrl: new URL("http://127.0.0.1:13381"),
      } as never);

      const result = await hooks.tool.envoy_whoami.execute({}, {
        sessionID: "ses_no_hostname",
        directory: "/tmp",
        metadata: mock(() => {}),
      } as never);

      const parsed = JSON.parse(typeof result === "string" ? result : result.output);
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
      const pluginModule = await import("../server");
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
      const pluginModule = await import("../server");
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

describe("heartbeat refreshes all busy sessions (fix 1a)", () => {
  it("re-subscribes every session that has been busy, not just the most recent", async () => {
    const originalEnvoyUrl = process.env.ENVOY_URL;
    const originalHb = process.env.ENVOY_HEARTBEAT_MS;
    process.env.ENVOY_URL = "http://127.0.0.1:59999";
    process.env.ENVOY_HEARTBEAT_MS = "40";

    const subs: { id: string; t: number }[] = [];
    const originalFetch = globalThis.fetch;
    globalThis.fetch = (async (input: string | URL | Request, init?: RequestInit) => {
      const url = typeof input === "string" ? input : input instanceof URL ? input.href : input.url;
      if (url.includes("/v1/interests/subscribe") && init?.body) {
        const body = JSON.parse(init.body as string);
        subs.push({ id: body.session_id, t: Date.now() });
        return new Response(JSON.stringify({ session_id: body.session_id, topics: [] }), {
          status: 200,
          headers: { "Content-Type": "application/json" },
        });
      }
      if (url.includes("/v1/sessions")) {
        return new Response(JSON.stringify([]), {
          status: 200,
          headers: { "Content-Type": "application/json" },
        });
      }
      // Serve title lookups -> 404 (no title, avoids follow-up subscribe noise)
      if (url.includes("/session/")) return new Response("not found", { status: 404 });
      throw new Error("connection refused");
    }) as typeof fetch;

    let dispose: (() => void) | undefined;
    try {
      const pluginModule = await import("../server");
      const hooks = await pluginModule.default({
        serverUrl: new URL("http://127.0.0.1:13381/"),
      } as never);
      dispose = (hooks as { dispose?: () => void }).dispose;

      const busy = (id: string) =>
        hooks.event({
          event: {
            type: "session.status",
            properties: { sessionID: id, status: { type: "busy" } },
          },
        });
      await busy("ses_A");
      await busy("ses_B");

      // Settle (< one heartbeat tick): capture ses_A's count before heartbeats run
      await new Promise((r) => setTimeout(r, 30));
      const aStart = subs.filter((s) => s.id === "ses_A").length;

      // ~4 heartbeat ticks at 40ms
      await new Promise((r) => setTimeout(r, 180));
      const aEnd = subs.filter((s) => s.id === "ses_A").length;
      const bEnd = subs.filter((s) => s.id === "ses_B").length;

      // ses_A is now idle (ses_B is the most-recently-busy). The heartbeat must
      // keep refreshing ses_A's registration, not only ses_B's.
      expect(aEnd).toBeGreaterThan(aStart);
      expect(bEnd).toBeGreaterThan(1);
    } finally {
      dispose?.();
      globalThis.fetch = originalFetch;
      process.env.ENVOY_URL = originalEnvoyUrl;
      if (originalHb === undefined) delete process.env.ENVOY_HEARTBEAT_MS;
      else process.env.ENVOY_HEARTBEAT_MS = originalHb;
    }
  });
});

describe("re-adopts sibling sessions after serve restart (fix 1b)", () => {
  it("registers idle same-dir+machine siblings on first activity, ignoring other machines/dirs", async () => {
    const originalEnvoyUrl = process.env.ENVOY_URL;
    process.env.ENVOY_URL = "http://127.0.0.1:59999";
    const cwd = process.cwd();

    const subscribed: string[] = [];
    const originalFetch = globalThis.fetch;
    globalThis.fetch = (async (input: string | URL | Request, init?: RequestInit) => {
      const url = typeof input === "string" ? input : input instanceof URL ? input.href : input.url;
      if (url.includes("/v1/interests/subscribe") && init?.body) {
        const body = JSON.parse(init.body as string);
        subscribed.push(body.session_id);
        return new Response(JSON.stringify({ session_id: body.session_id, topics: [] }), {
          status: 200,
          headers: { "Content-Type": "application/json" },
        });
      }
      if (url.includes("/v1/sessions")) {
        return new Response(
          JSON.stringify([
            {
              session_id: "ses_active",
              machine_id: "M",
              dir: cwd,
              port: 13381,
              title: "",
              topics: [],
              updated_at: Date.now(),
            },
            {
              session_id: "ses_idle",
              machine_id: "M",
              dir: cwd,
              port: 9,
              title: "Idle",
              topics: [],
              updated_at: Date.now(),
            },
            {
              session_id: "ses_foreign",
              machine_id: "OTHER",
              dir: cwd,
              port: 7,
              title: "",
              topics: [],
              updated_at: Date.now(),
            },
            {
              session_id: "ses_otherdir",
              machine_id: "M",
              dir: "/somewhere/else",
              port: 8,
              title: "",
              topics: [],
              updated_at: Date.now(),
            },
          ]),
          { status: 200, headers: { "Content-Type": "application/json" } }
        );
      }
      if (url.includes("/session/")) return new Response("not found", { status: 404 });
      throw new Error("connection refused");
    }) as typeof fetch;

    try {
      const pluginModule = await import("../server");
      const hooks = await pluginModule.default({
        serverUrl: new URL("http://127.0.0.1:13381/"),
      } as never);

      await hooks.event({
        event: {
          type: "session.status",
          properties: { sessionID: "ses_active", status: { type: "busy" } },
        },
      });
      await new Promise((r) => setTimeout(r, 100));

      expect(subscribed).toContain("ses_active");
      expect(subscribed).toContain("ses_idle");
      expect(subscribed).not.toContain("ses_foreign");
      expect(subscribed).not.toContain("ses_otherdir");
    } finally {
      globalThis.fetch = originalFetch;
      process.env.ENVOY_URL = originalEnvoyUrl;
    }
  });
});

describe("prunes deleted sessions from the heartbeat (fix 2)", () => {
  it("stops re-subscribing a session after session.deleted", async () => {
    const originalEnvoyUrl = process.env.ENVOY_URL;
    const originalHb = process.env.ENVOY_HEARTBEAT_MS;
    process.env.ENVOY_URL = "http://127.0.0.1:59999";
    process.env.ENVOY_HEARTBEAT_MS = "40";

    const subs: string[] = [];
    const originalFetch = globalThis.fetch;
    globalThis.fetch = (async (input: string | URL | Request, init?: RequestInit) => {
      const url = typeof input === "string" ? input : input instanceof URL ? input.href : input.url;
      if (url.includes("/v1/interests/subscribe") && init?.body) {
        const body = JSON.parse(init.body as string);
        subs.push(body.session_id);
        return new Response(JSON.stringify({ session_id: body.session_id, topics: [] }), {
          status: 200,
          headers: { "Content-Type": "application/json" },
        });
      }
      if (url.includes("/v1/interests/unsubscribe") || url.includes("/v1/sessions")) {
        return new Response(JSON.stringify([]), {
          status: 200,
          headers: { "Content-Type": "application/json" },
        });
      }
      if (url.includes("/session/")) return new Response("not found", { status: 404 });
      throw new Error("connection refused");
    }) as typeof fetch;

    let dispose: (() => void) | undefined;
    try {
      const pluginModule = await import("../server");
      const hooks = await pluginModule.default({
        serverUrl: new URL("http://127.0.0.1:13381/"),
      } as never);
      dispose = (hooks as { dispose?: () => void }).dispose;
      const busy = (id: string) =>
        hooks.event({
          event: {
            type: "session.status",
            properties: { sessionID: id, status: { type: "busy" } },
          },
        });
      await busy("ses_A");
      await busy("ses_B");

      await hooks.event({ event: { type: "session.deleted", properties: { sessionID: "ses_A" } } });
      // Let any in-flight heartbeat settle, then mark counts.
      await new Promise((r) => setTimeout(r, 60));
      const aMark = subs.filter((s) => s === "ses_A").length;
      const bMark = subs.filter((s) => s === "ses_B").length;

      await new Promise((r) => setTimeout(r, 160));
      const aEnd = subs.filter((s) => s === "ses_A").length;
      const bEnd = subs.filter((s) => s === "ses_B").length;

      // ses_A was deleted -> heartbeat must stop refreshing it.
      expect(aEnd).toBe(aMark);
      // ses_B is still alive -> heartbeat keeps refreshing it.
      expect(bEnd).toBeGreaterThan(bMark);
    } finally {
      dispose?.();
      globalThis.fetch = originalFetch;
      process.env.ENVOY_URL = originalEnvoyUrl;
      if (originalHb === undefined) delete process.env.ENVOY_HEARTBEAT_MS;
      else process.env.ENVOY_HEARTBEAT_MS = originalHb;
    }
  });
});

describe("re-adoption retries until the registry shows our own session (fix 3)", () => {
  it("adopts an idle sibling once /v1/sessions includes self on a later poll", async () => {
    const originalEnvoyUrl = process.env.ENVOY_URL;
    const originalHb = process.env.ENVOY_HEARTBEAT_MS;
    process.env.ENVOY_URL = "http://127.0.0.1:59999";
    process.env.ENVOY_HEARTBEAT_MS = "40";
    const cwd = process.cwd();

    let sessionsCalls = 0;
    const subscribed: string[] = [];
    const originalFetch = globalThis.fetch;
    globalThis.fetch = (async (input: string | URL | Request, init?: RequestInit) => {
      const url = typeof input === "string" ? input : input instanceof URL ? input.href : input.url;
      if (url.includes("/v1/interests/subscribe") && init?.body) {
        const body = JSON.parse(init.body as string);
        subscribed.push(body.session_id);
        return new Response(JSON.stringify({ session_id: body.session_id, topics: [] }), {
          status: 200,
          headers: { "Content-Type": "application/json" },
        });
      }
      if (url.includes("/v1/sessions")) {
        sessionsCalls += 1;
        // First poll: self not persisted yet. Later polls: self + idle sibling present.
        const body =
          sessionsCalls <= 1
            ? []
            : [
                {
                  session_id: "ses_active",
                  machine_id: "M",
                  dir: cwd,
                  port: 13381,
                  title: "",
                  topics: [],
                  updated_at: Date.now(),
                },
                {
                  session_id: "ses_idle",
                  machine_id: "M",
                  dir: cwd,
                  port: 9,
                  title: "Idle",
                  topics: [],
                  updated_at: Date.now(),
                },
              ];
        return new Response(JSON.stringify(body), {
          status: 200,
          headers: { "Content-Type": "application/json" },
        });
      }
      if (url.includes("/session/")) return new Response("not found", { status: 404 });
      throw new Error("connection refused");
    }) as typeof fetch;

    let dispose: (() => void) | undefined;
    try {
      const pluginModule = await import("../server");
      const hooks = await pluginModule.default({
        serverUrl: new URL("http://127.0.0.1:13381/"),
      } as never);
      dispose = (hooks as { dispose?: () => void }).dispose;
      await hooks.event({
        event: {
          type: "session.status",
          properties: { sessionID: "ses_active", status: { type: "busy" } },
        },
      });
      await new Promise((r) => setTimeout(r, 200));

      expect(subscribed).toContain("ses_idle");
    } finally {
      dispose?.();
      globalThis.fetch = originalFetch;
      process.env.ENVOY_URL = originalEnvoyUrl;
      if (originalHb === undefined) delete process.env.ENVOY_HEARTBEAT_MS;
      else process.env.ENVOY_HEARTBEAT_MS = originalHb;
    }
  });
});

describe("invalid ENVOY_HEARTBEAT_MS falls back to the default (fix 6)", () => {
  it("does not hammer subscribe when the env value is negative", async () => {
    const originalEnvoyUrl = process.env.ENVOY_URL;
    const originalHb = process.env.ENVOY_HEARTBEAT_MS;
    process.env.ENVOY_URL = "http://127.0.0.1:59999";
    process.env.ENVOY_HEARTBEAT_MS = "-5";

    const subs: string[] = [];
    const originalFetch = globalThis.fetch;
    globalThis.fetch = (async (input: string | URL | Request, init?: RequestInit) => {
      const url = typeof input === "string" ? input : input instanceof URL ? input.href : input.url;
      if (url.includes("/v1/interests/subscribe") && init?.body) {
        const body = JSON.parse(init.body as string);
        subs.push(body.session_id);
        return new Response(JSON.stringify({ session_id: body.session_id, topics: [] }), {
          status: 200,
          headers: { "Content-Type": "application/json" },
        });
      }
      if (url.includes("/v1/sessions")) {
        return new Response(JSON.stringify([]), {
          status: 200,
          headers: { "Content-Type": "application/json" },
        });
      }
      if (url.includes("/session/")) return new Response("not found", { status: 404 });
      throw new Error("connection refused");
    }) as typeof fetch;

    try {
      const pluginModule = await import("../server");
      const hooks = await pluginModule.default({
        serverUrl: new URL("http://127.0.0.1:13381/"),
      } as never);
      await hooks.event({
        event: {
          type: "session.status",
          properties: { sessionID: "ses_A", status: { type: "busy" } },
        },
      });
      await new Promise((r) => setTimeout(r, 200));

      // A negative interval must NOT be honored (would hammer); only the initial
      // subscribe should have happened within this window.
      expect(subs.filter((s) => s === "ses_A").length).toBe(1);
    } finally {
      globalThis.fetch = originalFetch;
      process.env.ENVOY_URL = originalEnvoyUrl;
      if (originalHb === undefined) delete process.env.ENVOY_HEARTBEAT_MS;
      else process.env.ENVOY_HEARTBEAT_MS = originalHb;
    }
  });
});

describe("tool.execute.after auto-subscribes the caller to dispatch threads (AC#4)", () => {
  async function runHook(tool: string, output: string): Promise<string[][]> {
    const originalEnvoyUrl = process.env.ENVOY_URL;
    process.env.ENVOY_URL = "http://127.0.0.1:59999";
    const subscribed: string[][] = [];
    const originalFetch = globalThis.fetch;
    globalThis.fetch = (async (input: string | URL | Request, init?: RequestInit) => {
      const url = typeof input === "string" ? input : input instanceof URL ? input.href : input.url;
      if (url.includes("/v1/interests/subscribe") && init?.body) {
        const body = JSON.parse(init.body as string) as { session_id: string; topics: string[] };
        subscribed.push([body.session_id, ...body.topics]);
        return new Response(JSON.stringify({ topics: [] }), {
          status: 200,
          headers: { "Content-Type": "application/json" },
        });
      }
      if (url.includes("/session/")) return new Response("not found", { status: 404 });
      return new Response("[]", { status: 200, headers: { "Content-Type": "application/json" } });
    }) as typeof fetch;
    try {
      const pluginModule = await import("../server");
      const hooks = await pluginModule.default({
        serverUrl: new URL("http://127.0.0.1:13381/"),
      } as never);
      const after = hooks["tool.execute.after"];
      expect(after).toBeDefined();
      await after?.(
        { tool, sessionID: "ses_dispatch", callID: "call_1", args: {} },
        { title: "Dispatch", output, metadata: {} }
      );
      return subscribed;
    } finally {
      globalThis.fetch = originalFetch;
      process.env.ENVOY_URL = originalEnvoyUrl;
    }
  }

  it("subscribes the calling session to the new thread's GitHub topic", async () => {
    const output = JSON.stringify({
      thread: 742,
      url: "https://github.com/sjawhar/legion/issues/742",
    });
    const subscribed = await runHook("envoy_dispatch", output);
    expect(subscribed).toContainEqual([
      "ses_dispatch",
      "notifications.github.sjawhar.legion.issue.742.>",
    ]);
  });

  it("does not subscribe for unrelated tools", async () => {
    const output = JSON.stringify({
      url: "https://github.com/sjawhar/legion/issues/9",
    });
    const subscribed = await runHook("envoy_subscribe", output);
    expect(subscribed.length).toBe(0);
  });
});

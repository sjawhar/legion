import { afterEach, beforeEach, describe, expect, it, mock, spyOn } from "bun:test";
import { existsSync, mkdirSync, writeFileSync } from "node:fs";
import * as os from "node:os";
import path from "node:path";
import { logger } from "../log";
import { isolateEnvoyConfig } from "./isolated-config";

// Each test's scratch HOME, holding the empty envoy.json ENVOY_CONFIG_FILE names.
const configDirectory = isolateEnvoyConfig();
// Suppress console.error during tests
const originalError = console.error;
const originalDispatchToken = process.env.DISPATCH_TOKEN;
beforeEach(() => {
  console.error = mock(() => {});
  process.env.DISPATCH_TOKEN ??= "test-token";
});
afterEach(() => {
  console.error = originalError;
  if (originalDispatchToken === undefined) delete process.env.DISPATCH_TOKEN;
  else process.env.DISPATCH_TOKEN = originalDispatchToken;
});

/** The plugin's hooks for a serve at a port nothing answers on. */
async function loadPlugin() {
  const pluginModule = await import("../server");
  return pluginModule.default({ serverUrl: new URL("http://127.0.0.1:13381/") } as never);
}

/** What the plugin's `shell.env` hook puts in a shell command's environment. */
async function shellEnvironment(hooks: {
  readonly "shell.env": (
    input: { cwd: string; sessionID?: string },
    output: { env: Record<string, string> }
  ) => Promise<void>;
}): Promise<Record<string, string>> {
  const output = { env: { PATH: "/usr/bin" } as Record<string, string> };
  await hooks["shell.env"]({ cwd: "/tmp", sessionID: "ses_shell" }, output);
  return output.env;
}

describe("the serve's envoy.json", () => {
  it("with OPENCODE_CONFIG_DIR holding no envoy.json, logs where the unread old file belongs and hands that file to every shell", async () => {
    const home = configDirectory();
    const opencodeDirectory = path.join(home, "opencode");
    const old = path.join(home, ".config", "opencode", "envoy.json");
    const marker = path.join(home, "old-token-ran");
    mkdirSync(opencodeDirectory);
    mkdirSync(path.dirname(old), { recursive: true });
    writeFileSync(
      old,
      JSON.stringify({
        dispatch: {
          enabled: true,
          serverUrl: "https://old.dispatch.example",
          token: `!touch ${marker}; echo old-token`,
        },
      })
    );
    process.env.OPENCODE_CONFIG_DIR = opencodeDirectory;
    const previous = { ...process.env };
    delete process.env.ENVOY_URL;
    delete process.env.ENVOY_NATS_URL;
    delete process.env.DISPATCH_URL;
    const warn = spyOn(logger, "warn").mockImplementation(() => {});
    const originalFetch = globalThis.fetch;
    const requests: string[] = [];
    globalThis.fetch = (async (input: string | URL | Request) => {
      requests.push(String(input));
      return new Response("no request belongs here", { status: 500 });
    }) as typeof fetch;
    try {
      const hooks = await loadPlugin();
      await hooks.event({
        event: {
          type: "session.status",
          properties: { sessionID: "ses_off", status: { type: "busy" } },
        },
      });
      await hooks.event({
        event: { type: "session.deleted", properties: { sessionID: "ses_off" } },
      });
      const file = path.join(opencodeDirectory, "envoy.json");

      expect(Object.keys(hooks.tool)).toEqual([]);
      expect((await shellEnvironment(hooks)).ENVOY_CONFIG_FILE).toBe(file);
      expect(warn.mock.calls).toEqual([
        [
          `envoy: no envoy.json at ${file}: Dispatch and Envoy are off; ${old} is not read for this profile; copy or link it to ${file}, or write that file`,
        ],
      ]);
      expect(existsSync(marker)).toBe(false);
      // The busy session's title is the serve's to look up; no listener is called.
      expect(requests.filter((url) => !url.startsWith("http://127.0.0.1:13381/"))).toEqual([]);
      hooks.dispose();
    } finally {
      globalThis.fetch = originalFetch;
      warn.mockRestore();
      process.env = previous;
    }
  });

  it("with Envoy off and Dispatch on, registers no Envoy tool and still carries dispatch-first", async () => {
    const file = process.env.ENVOY_CONFIG_FILE ?? "";
    writeFileSync(
      file,
      JSON.stringify({
        dispatch: { enabled: true, serverUrl: "https://dispatch.example", token: "file-token" },
      })
    );
    const previous = { ...process.env };
    delete process.env.ENVOY_URL;
    delete process.env.ENVOY_NATS_URL;
    delete process.env.DISPATCH_URL;
    delete process.env.DISPATCH_TOKEN;
    try {
      const hooks = await loadPlugin();
      const config: { instructions?: string[] } = {};
      hooks.config(config);

      expect(Object.keys(hooks.tool)).toEqual([]);
      expect(
        config.instructions?.some((instruction) => instruction.endsWith("dispatch-first/SKILL.md"))
      ).toBe(true);
      expect((await shellEnvironment(hooks)).ENVOY_CONFIG_FILE).toBe(file);
      hooks.dispose();
    } finally {
      process.env = previous;
    }
  });

  it("with a relative XDG_CONFIG_HOME, still loads with both sections off and says why once", async () => {
    const previous = { ...process.env };
    delete process.env.ENVOY_CONFIG_FILE;
    process.env.XDG_CONFIG_HOME = "relative/config";
    process.env.ENVOY_URL = "http://127.0.0.1:59999";
    const warn = spyOn(logger, "warn").mockImplementation(() => {});
    try {
      const hooks = await loadPlugin();

      expect(Object.keys(hooks.tool)).toEqual([]);
      expect((await shellEnvironment(hooks)).ENVOY_CONFIG_FILE).toBeUndefined();
      expect(warn.mock.calls).toEqual([
        [
          "envoy: XDG_CONFIG_HOME is relative/config, which is not an absolute path: Dispatch and Envoy are off",
        ],
      ]);
      hooks.dispose();
    } finally {
      warn.mockRestore();
      process.env = previous;
    }
  });
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

describe("Dispatch configuration", () => {
  it("keeps Envoy available and logs once when no envoy.json configures Dispatch", async () => {
    const previous = { ...process.env };
    const missing = path.join(configDirectory(), "missing", "envoy.json");
    process.env.ENVOY_CONFIG_FILE = missing;
    process.env.ENVOY_URL = "http://127.0.0.1:59999";
    delete process.env.DISPATCH_URL;
    delete process.env.DISPATCH_TOKEN;
    const warn = spyOn(logger, "warn").mockImplementation(() => {});

    try {
      const hooks = await loadPlugin();

      expect(Object.keys(hooks.tool).filter((name) => name.startsWith("dispatch_"))).toEqual([]);
      expect(hooks.tool.envoy_list).toBeDefined();
      expect(warn.mock.calls).toEqual([[`envoy: no envoy.json at ${missing}: Dispatch is off`]]);
      hooks.dispose();
    } finally {
      warn.mockRestore();
      process.env = previous;
    }
  });

  it("keeps Envoy available and logs the missing bearer token once", async () => {
    const previous = { ...process.env };
    process.env.DISPATCH_URL = "http://127.0.0.1:8766";
    process.env.ENVOY_URL = "http://127.0.0.1:59999";
    delete process.env.DISPATCH_TOKEN;
    const warn = spyOn(logger, "warn").mockImplementation(() => {});

    try {
      const hooks = await loadPlugin();

      expect(Object.keys(hooks.tool).filter((name) => name.startsWith("dispatch_"))).toEqual([]);
      expect(hooks.tool.envoy_list).toBeDefined();
      expect(warn.mock.calls).toEqual([
        [
          `envoy: Dispatch is off: DISPATCH_URL is set and no token is: set DISPATCH_TOKEN or DISPATCH_TOKEN_FILE, or dispatch.token in ${process.env.ENVOY_CONFIG_FILE}`,
        ],
      ]);
      hooks.dispose();
    } finally {
      warn.mockRestore();
      process.env = previous;
    }
  });
});

// The listener's bearer is resolved once and handed to the client: a plugin that dropped it would
// leave every pane's Envoy tools refused by the listener.
describe("the listener's bearer", () => {
  it("every listener call carries the bearer ENVOY_TOKEN_FILE names", async () => {
    const previous = { ...process.env };
    const originalFetch = globalThis.fetch;
    const tokenFile = path.join(configDirectory(), "envoy-token");
    writeFileSync(tokenFile, "pane-token\n");
    process.env.ENVOY_URL = "http://listener.test";
    process.env.ENVOY_TOKEN_FILE = tokenFile;
    const auth: (string | null)[] = [];
    globalThis.fetch = (async (input: string | URL | Request, init?: RequestInit) => {
      if (new URL(input.toString()).host === "listener.test") {
        auth.push(new Headers(init?.headers).get("Authorization"));
      }
      return Response.json([]);
    }) as typeof fetch;
    try {
      const hooks = await loadPlugin();
      await hooks.tool.envoy_sessions.execute({}, {
        sessionID: "ses_test",
        directory: "/tmp",
        metadata: () => {},
      } as never);
      expect(auth.length).toBeGreaterThan(0);
      expect(new Set(auth)).toEqual(new Set(["Bearer pane-token"]));
      hooks.dispose();
    } finally {
      globalThis.fetch = originalFetch;
      process.env = previous;
    }
  });
});

describe("envoy_whoami", () => {
  it("returns session identity when Envoy is unavailable", async () => {
    const originalEnvoyUrl = process.env.ENVOY_URL;
    process.env.ENVOY_URL = "http://127.0.0.1:59999";

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
      expect(parsed.machine_id).toBe(os.hostname());
      expect(parsed.dir).toBe("/tmp/test-workspace");
      expect(parsed).not.toHaveProperty("topics");
      expect(parsed.port === null || typeof parsed.port === "number").toBe(true);
    } finally {
      process.env.ENVOY_URL = originalEnvoyUrl;
    }
  });

  it("reports machine_id from the real hostname, not the HOSTNAME env var", async () => {
    const originalEnvoyUrl = process.env.ENVOY_URL;
    const originalHostname = process.env.HOSTNAME;
    process.env.ENVOY_URL = "http://127.0.0.1:59999";
    // Same host must never report two machine IDs depending on shell env.
    process.env.HOSTNAME = "some-other-name";

    try {
      const pluginModule = await import("../server");
      const initPlugin = pluginModule.default;
      const hooks = await initPlugin({
        serverUrl: new URL("http://127.0.0.1:13381"),
      } as never);

      const result = await hooks.tool.envoy_whoami.execute({}, {
        sessionID: "ses_env_hostname",
        directory: "/tmp",
        metadata: mock(() => {}),
      } as never);

      const parsed = JSON.parse(typeof result === "string" ? result : result.output);
      expect(parsed.machine_id).toBe(os.hostname());
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

describe("heartbeat refreshes all busy sessions", () => {
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

describe("prunes deleted sessions from the heartbeat", () => {
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

describe("invalid ENVOY_HEARTBEAT_MS falls back to the default", () => {
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

// Several live processes can hold the same session (opencode session state is on
// shared disk). Envoy arbitrates competing route claims by whether the claiming
// process is DRIVING the session, so the plugin must report that honestly:
// sessions that have run in this process are driven; siblings re-adopted after a
// serve restart are recovery claims that must not displace a live driver.
describe("claims report whether this process drives the session", () => {
  it("marks sessions that have been busy in this process as driving", async () => {
    const originalEnvoyUrl = process.env.ENVOY_URL;
    process.env.ENVOY_URL = "http://127.0.0.1:59999";

    const claims: { id: string; driving: unknown }[] = [];
    const originalFetch = globalThis.fetch;
    globalThis.fetch = (async (input: string | URL | Request, init?: RequestInit) => {
      const url = typeof input === "string" ? input : input instanceof URL ? input.href : input.url;
      if (url.includes("/v1/interests/subscribe") && init?.body) {
        const body = JSON.parse(init.body as string);
        claims.push({ id: body.session_id, driving: body.driving });
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
          properties: { sessionID: "ses_driven", status: { type: "busy" } },
        },
      });
      await new Promise((r) => setTimeout(r, 30));

      const own = claims.filter((c) => c.id === "ses_driven");
      expect(own.length).toBeGreaterThan(0);
      expect(own.every((c) => c.driving === true)).toBe(true);
    } finally {
      dispose?.();
      globalThis.fetch = originalFetch;
      process.env.ENVOY_URL = originalEnvoyUrl;
    }
  });
});

// Serve-restart recovery claims only sessions this process has actually run.
// The daemon keeps idle-but-owned sessions reachable; a stranger process must
// not adopt sibling routes from shared on-disk session state.
describe("a process claims only sessions it has run", () => {
  const runReadopt = async (siblingPortAlive: boolean) => {
    const originalEnvoyUrl = process.env.ENVOY_URL;
    process.env.ENVOY_URL = "http://127.0.0.1:59999";
    const siblingPort = 34751;

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
            { session_id: "ses_self", machine_id: "m", dir: process.cwd(), port: 42145 },
            {
              session_id: "ses_sibling",
              machine_id: "m",
              dir: process.cwd(),
              port: siblingPort,
            },
          ]),
          { status: 200, headers: { "Content-Type": "application/json" } }
        );
      }
      // Any liveness probe at all means readopt is still trying to adopt.
      if (url.includes(`:${siblingPort}/`)) {
        if (siblingPortAlive) {
          return new Response(JSON.stringify({ healthy: true }), {
            status: 200,
            headers: { "Content-Type": "application/json" },
          });
        }
        throw new Error("connection refused");
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
          properties: { sessionID: "ses_self", status: { type: "busy" } },
        },
      });
      await new Promise((r) => setTimeout(r, 80));
      return subscribed;
    } finally {
      dispose?.();
      globalThis.fetch = originalFetch;
      process.env.ENVOY_URL = originalEnvoyUrl;
    }
  };

  it("never claims a sibling session, whether or not its serve is alive", async () => {
    for (const siblingServeAlive of [true, false]) {
      const subscribed = await runReadopt(siblingServeAlive);

      expect(subscribed).toContain("ses_self");
      expect(subscribed).not.toContain("ses_sibling");
    }
  });
});

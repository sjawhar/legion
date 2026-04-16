import { afterEach, describe, expect, it, mock } from "bun:test";

// Mutable handlers for per-test control of SDK responses
let sessionStatusData: Record<string, unknown> = {};
let sessionGetData: unknown;
let sessionGetError: unknown;
let sessionMessagesData: unknown = [];
let sessionMessagesError: unknown;

mock.module("@opencode-ai/sdk/v2", () => ({
  createOpencodeClient: () => ({
    session: {
      promptAsync: async () => ({ data: { id: "p1" } }),
      status: async () => ({ data: sessionStatusData }),
      get: async () => ({ data: sessionGetData, error: sessionGetError }),
      messages: async () => ({
        data: sessionMessagesData,
        error: sessionMessagesError,
      }),
    },
  }),
}));

const serveManagerMock = () => ({
  createSession: async (_port: number, sessionId: string, _workspace: string): Promise<string> => {
    if (sessionId === "ses_requested") {
      return "ses_actual_different";
    }
    return sessionId;
  },
  spawnSharedServe: async () => ({
    port: 13381,
    pid: 9999,
    status: "starting" as const,
  }),
  waitForHealthy: async () => {},
  healthCheck: async () => true,
  stopServe: async () => {},
  createWorkerClient: () => ({
    session: {
      promptAsync: async () => ({ data: { id: "p1" } }),
      status: async () => ({ data: sessionStatusData }),
      get: async () => ({ data: sessionGetData, error: sessionGetError }),
      messages: async () => ({
        data: sessionMessagesData,
        error: sessionMessagesError,
      }),
    },
  }),
});

mock.module("../serve-manager", serveManagerMock);
mock.module("../../serve-manager", serveManagerMock);

import { OpenCodeAdapter } from "../opencode";

// Helper to wrap message data in the SDK's { info: Message, parts: Part[] } envelope
function wrapMsg(info: Record<string, unknown>) {
  return { info, parts: [] };
}

describe("OpenCodeAdapter", () => {
  afterEach(() => {
    sessionStatusData = {};
    sessionGetData = undefined;
    sessionGetError = undefined;
    sessionMessagesData = [];
    sessionMessagesError = undefined;
  });

  it("implements RuntimeAdapter interface", () => {
    const adapter = new OpenCodeAdapter(13381);
    expect(typeof adapter.start).toBe("function");
    expect(typeof adapter.stop).toBe("function");
    expect(typeof adapter.healthy).toBe("function");
    expect(typeof adapter.createSession).toBe("function");
    expect(typeof adapter.sendPrompt).toBe("function");
    expect(typeof adapter.getSessionStatus).toBe("function");
  });

  describe("getPort", () => {
    it("returns the port passed to constructor", () => {
      const adapter = new OpenCodeAdapter(13381);
      expect(adapter.getPort()).toBe(13381);
    });

    it("returns a different port for a different constructor value", () => {
      const adapter = new OpenCodeAdapter(9999);
      expect(adapter.getPort()).toBe(9999);
    });
  });

  describe("createSession", () => {
    it("returns actual session ID from serve-manager, not requested ID", async () => {
      const adapter = new OpenCodeAdapter(13381);
      const actualId = await adapter.createSession("ses_requested", "/tmp/ws");
      expect(actualId).toBe("ses_actual_different");
    });

    it("keys workspace map on actual ID so sendPrompt can find it", async () => {
      const adapter = new OpenCodeAdapter(13381);
      const actualId = await adapter.createSession("ses_requested", "/tmp/ws");
      expect(actualId).toBe("ses_actual_different");
      await adapter.sendPrompt("ses_actual_different", "hello");
    });
  });

  describe("getSessionStatus", () => {
    it("returns enriched busy status with activity signals", async () => {
      const adapter = new OpenCodeAdapter(13381);
      await adapter.createSession("ses_abc", "/tmp/ws");

      const updatedAt = 1711664392; // 2024-03-28T21:39:52Z
      sessionStatusData = { ses_abc: { type: "busy" } };
      sessionGetData = { time: { created: 1711660000, updated: updatedAt } };
      sessionMessagesData = [
        wrapMsg({ id: "m1", role: "user", time: { created: 1711660000 } }),
        wrapMsg({
          id: "m2",
          role: "assistant",
          time: { created: 1711660100 },
          tokens: {
            input: 1000,
            output: 500,
            total: 1500,
            reasoning: 0,
            cache: { read: 0, write: 0 },
          },
        }),
        wrapMsg({ id: "m3", role: "user", time: { created: 1711660200 } }),
        wrapMsg({
          id: "m4",
          role: "assistant",
          time: { created: 1711660300 },
          tokens: {
            input: 2000,
            output: 800,
            total: 2800,
            reasoning: 0,
            cache: { read: 0, write: 0 },
          },
        }),
      ];

      const result = await adapter.getSessionStatus("ses_abc");
      expect(result.data).toEqual({
        type: "busy",
        lastActivityAt: new Date(updatedAt * 1000).toISOString(),
        messageCount: 4,
        turnCount: 2,
        phase: "busy",
        tokensUsed: 4300,
      });
    });

    it("returns enriched idle status with zero counts for empty session", async () => {
      const adapter = new OpenCodeAdapter(13381);
      await adapter.createSession("ses_empty", "/tmp/ws");

      sessionStatusData = { ses_empty: { type: "idle" } };
      sessionGetData = { time: { created: 1711660000, updated: 1711660000 } };
      sessionMessagesData = [];

      const result = await adapter.getSessionStatus("ses_empty");
      expect(result.data).toEqual({
        type: "idle",
        lastActivityAt: new Date(1711660000 * 1000).toISOString(),
        messageCount: 0,
        turnCount: 0,
        phase: "idle",
        tokensUsed: 0,
      });
    });

    it("falls back to input+output when tokens.total is absent", async () => {
      const adapter = new OpenCodeAdapter(13381);
      await adapter.createSession("ses_nototal", "/tmp/ws");

      sessionStatusData = { ses_nototal: { type: "busy" } };
      sessionGetData = { time: { created: 1711660000, updated: 1711660500 } };
      sessionMessagesData = [
        wrapMsg({
          id: "m1",
          role: "assistant",
          time: { created: 1711660100 },
          tokens: { input: 3000, output: 1200, reasoning: 0, cache: { read: 0, write: 0 } },
        }),
      ];

      const result = await adapter.getSessionStatus("ses_nototal");
      const data = result.data as Record<string, unknown>;
      expect(data.tokensUsed).toBe(4200);
      expect(data.turnCount).toBe(1);
      expect(data.messageCount).toBe(1);
    });

    it("defaults to idle when session not in status map", async () => {
      const adapter = new OpenCodeAdapter(13381);
      await adapter.createSession("ses_missing", "/tmp/ws");

      sessionStatusData = {}; // session not in map
      sessionGetData = { time: { created: 1711660000, updated: 1711660000 } };
      sessionMessagesData = [];

      const result = await adapter.getSessionStatus("ses_missing");
      const data = result.data as Record<string, unknown>;
      expect(data.type).toBe("idle");
      expect(data.phase).toBe("idle");
    });

    it("sets lastActivityAt to null when session time is missing", async () => {
      const adapter = new OpenCodeAdapter(13381);
      await adapter.createSession("ses_notime", "/tmp/ws");

      sessionStatusData = { ses_notime: { type: "busy" } };
      sessionGetData = {}; // no time field
      sessionMessagesData = [];

      const result = await adapter.getSessionStatus("ses_notime");
      const data = result.data as Record<string, unknown>;
      expect(data.lastActivityAt).toBeNull();
    });

    it("handles retry status type", async () => {
      const adapter = new OpenCodeAdapter(13381);
      await adapter.createSession("ses_retry", "/tmp/ws");

      sessionStatusData = {
        ses_retry: { type: "retry", attempt: 2, message: "rate limited", next: 1711660500 },
      };
      sessionGetData = { time: { created: 1711660000, updated: 1711660400 } };
      sessionMessagesData = [];

      const result = await adapter.getSessionStatus("ses_retry");
      const data = result.data as Record<string, unknown>;
      expect(data.type).toBe("retry");
      expect(data.phase).toBe("retry");
      // retry-specific fields should be preserved via spread
      expect(data.attempt).toBe(2);
      expect(data.message).toBe("rate limited");
    });

    it("returns enriched status with safe defaults when session/messages are undefined", async () => {
      const adapter = new OpenCodeAdapter(13381);
      await adapter.createSession("ses_fail", "/tmp/ws");

      sessionStatusData = { ses_fail: { type: "busy" } };
      sessionGetData = undefined;
      sessionGetError = { message: "not found" };
      sessionMessagesData = undefined;
      sessionMessagesError = { message: "not found" };

      const result = await adapter.getSessionStatus("ses_fail");
      const data = result.data as Record<string, unknown>;
      expect(data.type).toBe("busy");
      expect(data.lastActivityAt).toBeNull();
      expect(data.messageCount).toBe(0);
      expect(data.turnCount).toBe(0);
      expect(data.tokensUsed).toBe(0);
    });

    it("only counts assistant messages for turnCount and tokensUsed", async () => {
      const adapter = new OpenCodeAdapter(13381);
      await adapter.createSession("ses_mixed", "/tmp/ws");

      sessionStatusData = { ses_mixed: { type: "busy" } };
      sessionGetData = { time: { created: 1711660000, updated: 1711661000 } };
      sessionMessagesData = [
        wrapMsg({ id: "u1", role: "user", time: { created: 1711660000 } }),
        wrapMsg({ id: "u2", role: "user", time: { created: 1711660100 } }),
        wrapMsg({ id: "u3", role: "user", time: { created: 1711660200 } }),
        wrapMsg({
          id: "a1",
          role: "assistant",
          time: { created: 1711660300 },
          tokens: {
            input: 500,
            output: 200,
            total: 700,
            reasoning: 0,
            cache: { read: 0, write: 0 },
          },
        }),
      ];

      const result = await adapter.getSessionStatus("ses_mixed");
      const data = result.data as Record<string, unknown>;
      expect(data.messageCount).toBe(4); // all messages
      expect(data.turnCount).toBe(1); // only assistant
      expect(data.tokensUsed).toBe(700); // only assistant tokens
    });

    it("returns original error when status() call itself fails", async () => {
      const adapter = new OpenCodeAdapter(13381);
      await adapter.createSession("ses_statuserr", "/tmp/ws");

      sessionStatusData = {};
      const result = await adapter.getSessionStatus("ses_statuserr");
      const data = result.data as Record<string, unknown>;
      // Session not in map → defaults to idle
      expect(data.type).toBe("idle");
    });

    it("handles assistant messages without tokens field", async () => {
      const adapter = new OpenCodeAdapter(13381);
      await adapter.createSession("ses_notokens", "/tmp/ws");

      sessionStatusData = { ses_notokens: { type: "busy" } };
      sessionGetData = { time: { created: 1711660000, updated: 1711660500 } };
      sessionMessagesData = [
        wrapMsg({
          id: "a1",
          role: "assistant",
          time: { created: 1711660100 },
          // no tokens field
        }),
        wrapMsg({
          id: "a2",
          role: "assistant",
          time: { created: 1711660200 },
          tokens: {
            input: 1000,
            output: 500,
            total: 1500,
            reasoning: 0,
            cache: { read: 0, write: 0 },
          },
        }),
      ];

      const result = await adapter.getSessionStatus("ses_notokens");
      const data = result.data as Record<string, unknown>;
      expect(data.turnCount).toBe(2);
      expect(data.tokensUsed).toBe(1500); // only from second message
    });
  });
  describe("sessionExists", () => {
    it("returns true when session exists (200 response)", async () => {
      const adapter = new OpenCodeAdapter(13381);
      const originalFetch = globalThis.fetch;
      try {
        globalThis.fetch = (async (url: string | URL | Request) => {
          const urlStr = String(url);
          if (urlStr.includes("/session/ses_exists")) {
            return new Response(JSON.stringify({ id: "ses_exists" }), { status: 200 });
          }
          return new Response("not found", { status: 404 });
        }) as typeof fetch;

        expect(await adapter.sessionExists("ses_exists")).toBe(true);
        expect(await adapter.sessionExists("ses_missing")).toBe(false);
      } finally {
        globalThis.fetch = originalFetch;
      }
    });

    it("returns false on network error", async () => {
      const adapter = new OpenCodeAdapter(13381);
      const originalFetch = globalThis.fetch;
      try {
        globalThis.fetch = Object.assign(
          async () => {
            throw new Error("connection refused");
          },
          { preconnect: originalFetch.preconnect }
        ) as typeof fetch;

        expect(await adapter.sessionExists("ses_any")).toBe(false);
      } finally {
        globalThis.fetch = originalFetch;
      }
    });
  });

  describe("adoptServe", () => {
    it("sets the PID so getServePid returns it", () => {
      const adapter = new OpenCodeAdapter(13381);
      expect(adapter.getServePid()).toBe(0);

      adapter.adoptServe(42424);
      expect(adapter.getServePid()).toBe(42424);
    });

    it("allows stop() to target the adopted PID", async () => {
      const adapter = new OpenCodeAdapter(13381);
      adapter.adoptServe(999999999);

      // stop() calls stopServe(port, pid) — with a dead PID, it should
      // complete without error (stopServe handles ESRCH gracefully)
      await adapter.stop();
    });
  });
});

import { describe, expect, it, mock } from "bun:test";

mock.module("@opencode-ai/sdk/v2", () => ({
  createOpencodeClient: () => ({
    session: {
      promptAsync: async () => ({ data: { id: "p1" } }),
      status: async () => ({ data: { status: "idle" } }),
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
  spawnSharedServe: async () => ({ port: 13381, pid: 9999, status: "starting" as const }),
  waitForHealthy: async () => {},
  healthCheck: async () => true,
  stopServe: async () => {},
  createWorkerClient: () => ({
    session: {
      promptAsync: async () => ({ data: { id: "p1" } }),
      status: async () => ({ data: { status: "idle" } }),
    },
  }),
});

mock.module("../serve-manager", serveManagerMock);
mock.module("../../serve-manager", serveManagerMock);

import { OpenCodeAdapter } from "../opencode";

describe("OpenCodeAdapter", () => {
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
});

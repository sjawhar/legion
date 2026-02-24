import { describe, expect, it, mock } from "bun:test";

mock.module("@opencode-ai/sdk/v2", () => ({
  createOpencodeClient: () => ({
    session: {
      promptAsync: async () => ({ data: { id: "p1" } }),
      status: async () => ({ data: { status: "idle" } }),
    },
  }),
}));

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
});

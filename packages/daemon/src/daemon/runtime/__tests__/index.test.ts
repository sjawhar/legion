import { describe, expect, it, mock } from "bun:test";

mock.module("@opencode-ai/sdk/v2", () => ({
  createOpencodeClient: () => ({
    session: {
      promptAsync: async () => ({ data: { id: "p1" } }),
      status: async () => ({ data: { status: "idle" } }),
    },
  }),
}));

import { createAdapter } from "../index";

describe("createAdapter", () => {
  it("returns OpenCodeAdapter for opencode runtime", () => {
    const adapter = createAdapter("opencode", { port: 13381, shortId: "test" });
    expect(adapter.getPort()).toBe(13381);
  });

  it("returns ClaudeCodeAdapter for claude-code runtime", () => {
    const adapter = createAdapter("claude-code", { port: 0, shortId: "test" });
    expect(adapter.getPort()).toBe(0);
  });
});
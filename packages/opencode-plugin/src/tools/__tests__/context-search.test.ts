import { describe, expect, it } from "bun:test";
import type { ToolContext } from "@opencode-ai/plugin";
import type { ContentStore } from "../../store/content-store";
import { createContextSearchTool } from "../context-search";

function makeContext(): ToolContext {
  return {
    sessionID: "session-1",
    messageID: "msg-1",
    agent: "orchestrator",
    directory: "/tmp",
    worktree: "/tmp",
    abort: new AbortController().signal,
    metadata: () => {},
    ask: async () => {},
  };
}

describe("context_search", () => {
  it("returns formatted results when store has matching content", async () => {
    const store = {
      search: () => [
        {
          source: "bash:call123",
          title: "Chunk 1",
          score: 0.876,
          content: "line 1\nline 2",
        },
      ],
    } as unknown as ContentStore;

    const tool = createContextSearchTool(store);
    const result = await tool.execute({ queries: ["line 1"] }, makeContext());

    expect(result).toContain("--- [bash:call123] Chunk 1 (score: 0.88) ---");
    expect(result).toContain("line 1");
    expect(result).toContain("line 2");
  });

  it("returns no matching content message when nothing matches", async () => {
    const store = {
      search: () => [],
    } as unknown as ContentStore;

    const tool = createContextSearchTool(store);
    const result = await tool.execute({ queries: ["missing"] }, makeContext());

    expect(result).toBe(
      "No matching content found. Try different search terms or check available sources with context_search."
    );
  });

  it("passes source filter through to store", async () => {
    let receivedSource: string | undefined;
    let receivedSession: string | undefined;
    const store = {
      search: (input: { queries: string[]; source?: string; session?: string; limit?: number }) => {
        receivedSource = input.source;
        receivedSession = input.session;
        return [];
      },
    } as unknown as ContentStore;

    const tool = createContextSearchTool(store);
    await tool.execute({ queries: ["worker"], source: "bash:call123" }, makeContext());

    expect(receivedSource).toBe("bash:call123");
    expect(receivedSession).toBe("session-1");
  });

  it("passes limit through to store", async () => {
    let receivedLimit: number | undefined;
    const store = {
      search: (input: { queries: string[]; source?: string; limit?: number }) => {
        receivedLimit = input.limit;
        return [];
      },
    } as unknown as ContentStore;

    const tool = createContextSearchTool(store);
    await tool.execute({ queries: ["worker"], limit: 7 }, makeContext());

    expect(receivedLimit).toBe(7);
  });

  it("handles errors gracefully", async () => {
    const store = {
      search: () => {
        throw new Error("store unavailable");
      },
    } as unknown as ContentStore;

    const tool = createContextSearchTool(store);
    const result = await tool.execute({ queries: ["worker"] }, makeContext());

    expect(result).toBe("Error: store unavailable");
  });

  it("uses default limit of 3", async () => {
    let receivedLimit: number | undefined;
    const store = {
      search: (input: { queries: string[]; source?: string; limit?: number }) => {
        receivedLimit = input.limit;
        return [];
      },
    } as unknown as ContentStore;

    const tool = createContextSearchTool(store);
    await tool.execute({ queries: ["worker"] }, makeContext());

    expect(receivedLimit).toBe(3);
  });

  it("passes caller's sessionID to store for session isolation", async () => {
    let receivedSession: string | undefined;
    const store = {
      search: (input: { queries: string[]; source?: string; session?: string; limit?: number }) => {
        receivedSession = input.session;
        return [];
      },
    } as unknown as ContentStore;

    const tool = createContextSearchTool(store);
    const ctx = makeContext();
    await tool.execute({ queries: ["worker"] }, ctx);

    expect(receivedSession).toBe("session-1");
  });
});

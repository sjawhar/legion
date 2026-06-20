import { describe, expect, it, mock } from "bun:test";
import { createBridge, type JsonRpcRequest } from "../dispatch-mcp-bridge";

interface MockResponse {
  status: number;
  statusText?: string;
  headers?: Record<string, string>;
  contentType?: string;
  body: string;
}

function fakeFetch(responses: MockResponse[]) {
  let idx = 0;
  const calls: Array<{ url: string; init: RequestInit }> = [];
  const impl = (url: string, init?: RequestInit) => {
    calls.push({ url, init: init ?? {} });
    const next = responses[idx++];
    if (!next) throw new Error(`no mock response for call #${idx}`);
    const headers = new Headers({
      "content-type": next.contentType ?? "application/json",
      ...(next.headers ?? {}),
    });
    return Promise.resolve(
      new Response(next.body, {
        status: next.status,
        statusText: next.statusText ?? "",
        headers,
      })
    );
  };
  return { impl: impl as unknown as typeof fetch, calls };
}

function sseEnvelope(payload: object): string {
  return `event: message\ndata: ${JSON.stringify(payload)}\n\n`;
}

describe("dispatch-mcp-bridge", () => {
  it("forwards a request with a fresh bearer and returns the parsed SSE response", async () => {
    const f = fakeFetch([
      {
        status: 200,
        contentType: "text/event-stream",
        headers: { "mcp-session-id": "S1" },
        body: sseEnvelope({ jsonrpc: "2.0", id: 1, result: { ok: true } }),
      },
    ]);
    const bridge = createBridge({
      remoteUrl: "http://example/mcp",
      getToken: async () => "tok-A",
      fetchImpl: f.impl,
    });

    const req: JsonRpcRequest = { jsonrpc: "2.0", id: 1, method: "tools/list" };
    const res = await bridge.handle(req);

    expect(res).toEqual({ jsonrpc: "2.0", id: 1, result: { ok: true } });
    expect(f.calls).toHaveLength(1);
    expect((f.calls[0]?.init.headers as Record<string, string>).Authorization).toBe("Bearer tok-A");
  });

  it("reuses the cached token on a follow-up request and reuses the session id", async () => {
    let tokenCalls = 0;
    const f = fakeFetch([
      {
        status: 200,
        contentType: "text/event-stream",
        headers: { "mcp-session-id": "S2" },
        body: sseEnvelope({ jsonrpc: "2.0", id: 1, result: { phase: "init" } }),
      },
      {
        status: 200,
        contentType: "text/event-stream",
        body: sseEnvelope({ jsonrpc: "2.0", id: 2, result: { phase: "list" } }),
      },
    ]);
    const bridge = createBridge({
      remoteUrl: "http://example/mcp",
      getToken: async () => {
        tokenCalls++;
        return "tok-cached";
      },
      fetchImpl: f.impl,
    });

    await bridge.handle({ jsonrpc: "2.0", id: 1, method: "initialize" });
    await bridge.handle({ jsonrpc: "2.0", id: 2, method: "tools/list" });

    expect(tokenCalls).toBe(1);
    expect((f.calls[1]?.init.headers as Record<string, string>)["Mcp-Session-Id"]).toBe("S2");
  });

  it("refreshes the token after the cache TTL elapses", async () => {
    const responses: MockResponse[] = [
      {
        status: 200,
        contentType: "text/event-stream",
        body: sseEnvelope({ jsonrpc: "2.0", id: 1, result: 1 }),
      },
      {
        status: 200,
        contentType: "text/event-stream",
        body: sseEnvelope({ jsonrpc: "2.0", id: 2, result: 2 }),
      },
    ];
    const f = fakeFetch(responses);
    let issued = 0;
    let clock = 1000;

    const bridge = createBridge({
      remoteUrl: "http://example/mcp",
      getToken: async () => `tok-${++issued}`,
      fetchImpl: f.impl,
      tokenCacheTtlMs: 60_000,
      now: () => clock,
    });

    await bridge.handle({ jsonrpc: "2.0", id: 1, method: "x" });
    clock += 120_000; // beyond TTL
    await bridge.handle({ jsonrpc: "2.0", id: 2, method: "x" });

    expect(issued).toBe(2);
    expect((f.calls[0]?.init.headers as Record<string, string>).Authorization).toBe("Bearer tok-1");
    expect((f.calls[1]?.init.headers as Record<string, string>).Authorization).toBe("Bearer tok-2");
  });

  it("retries once on 401 with a forced token refresh", async () => {
    const f = fakeFetch([
      { status: 401, body: "unauthorized" },
      {
        status: 200,
        contentType: "text/event-stream",
        body: sseEnvelope({ jsonrpc: "2.0", id: 1, result: "after-refresh" }),
      },
    ]);
    let issued = 0;
    const bridge = createBridge({
      remoteUrl: "http://example/mcp",
      getToken: async () => `tok-${++issued}`,
      fetchImpl: f.impl,
      logError: () => {},
    });

    const res = await bridge.handle({ jsonrpc: "2.0", id: 1, method: "tools/list" });
    expect(res).toEqual({ jsonrpc: "2.0", id: 1, result: "after-refresh" });
    expect(issued).toBe(2); // first attempt cached tok-1, retry forced tok-2
    expect(f.calls).toHaveLength(2);
  });

  it("retries when remote returns HTTP 200 but tool result reports upstream 401", async () => {
    const f = fakeFetch([
      {
        status: 200,
        contentType: "application/json",
        body: JSON.stringify({
          jsonrpc: "2.0",
          id: 1,
          result: {
            isError: true,
            content: [
              {
                type: "text",
                text: "search issues: GET https://api.github.com/search/issues?q=x: 401 Bad credentials []",
              },
            ],
          },
        }),
      },
      {
        status: 200,
        contentType: "text/event-stream",
        body: sseEnvelope({ jsonrpc: "2.0", id: 1, result: "after-refresh" }),
      },
    ]);
    let issued = 0;
    const bridge = createBridge({
      remoteUrl: "http://example/mcp",
      getToken: async () => `tok-${++issued}`,
      fetchImpl: f.impl,
      logError: () => {},
    });
    const res = await bridge.handle({ jsonrpc: "2.0", id: 1, method: "tools/call" });
    expect(res).toEqual({ jsonrpc: "2.0", id: 1, result: "after-refresh" });
    expect(issued).toBe(2);
    expect(f.calls).toHaveLength(2);
  });

  it("retries when remote returns a JSON-RPC error whose message reports upstream 401", async () => {
    const f = fakeFetch([
      {
        status: 200,
        contentType: "application/json",
        body: JSON.stringify({
          jsonrpc: "2.0",
          id: 1,
          error: { code: -32603, message: "search issues: 401 Bad credentials" },
        }),
      },
      {
        status: 200,
        contentType: "text/event-stream",
        body: sseEnvelope({ jsonrpc: "2.0", id: 1, result: "after-refresh" }),
      },
    ]);
    let issued = 0;
    const bridge = createBridge({
      remoteUrl: "http://example/mcp",
      getToken: async () => `tok-${++issued}`,
      fetchImpl: f.impl,
      logError: () => {},
    });
    const res = await bridge.handle({ jsonrpc: "2.0", id: 1, method: "tools/call" });
    expect(res).toEqual({ jsonrpc: "2.0", id: 1, result: "after-refresh" });
    expect(issued).toBe(2);
  });

  it("returns a JSON-RPC error when the token getter yields null", async () => {
    const f = fakeFetch([]);
    const bridge = createBridge({
      remoteUrl: "http://example/mcp",
      getToken: async () => null,
      fetchImpl: f.impl,
    });
    const res = await bridge.handle({ jsonrpc: "2.0", id: 7, method: "tools/call" });
    expect(res?.error?.code).toBe(-32000);
    expect(res?.error?.message).toContain("gh auth token");
    expect(f.calls).toHaveLength(0);
  });

  it("returns null for notifications (no id) and still forwards them", async () => {
    const f = fakeFetch([{ status: 200, contentType: "application/json", body: "{}" }]);
    const bridge = createBridge({
      remoteUrl: "http://example/mcp",
      getToken: async () => "tok",
      fetchImpl: f.impl,
    });
    const res = await bridge.handle({ jsonrpc: "2.0", method: "notifications/initialized" });
    expect(res).toBeNull();
    expect(f.calls).toHaveLength(1);
  });

  it("returns a JSON-RPC error on remote non-200 status", async () => {
    const f = fakeFetch([{ status: 503, statusText: "Service Unavailable", body: "down" }]);
    const bridge = createBridge({
      remoteUrl: "http://example/mcp",
      getToken: async () => "tok",
      fetchImpl: f.impl,
    });
    const res = await bridge.handle({ jsonrpc: "2.0", id: 1, method: "x" });
    expect(res?.error?.code).toBe(-32603);
    expect(res?.error?.message).toContain("503");
  });

  it("returns a JSON-RPC error when fetch throws", async () => {
    const erroringFetch = mock(async () => {
      throw new Error("ECONNREFUSED");
    });
    const bridge = createBridge({
      remoteUrl: "http://example/mcp",
      getToken: async () => "tok",
      fetchImpl: erroringFetch as unknown as typeof fetch,
    });
    const res = await bridge.handle({ jsonrpc: "2.0", id: 1, method: "x" });
    expect(res?.error?.code).toBe(-32603);
    expect(res?.error?.message).toContain("ECONNREFUSED");
  });

  it("normalizes union-type-null arrays in tools/list inputSchema so Gemini accepts them", async () => {
    // Mirrors the real EnvoyDispatch schema the remote server emits: nullable arrays
    // expressed as JSON-Schema union types { type: ["null", "array"] }, which Gemini rejects
    // (array branch lacks items / orphaned items). The bridge must collapse these in transit.
    const toolsList = {
      jsonrpc: "2.0",
      id: 1,
      result: {
        tools: [
          {
            name: "envoy_dispatch",
            description: "Create a Dispatch thread",
            inputSchema: {
              type: "object",
              required: ["parent", "subject", "body"],
              properties: {
                parent: { type: "string" },
                ask: {
                  type: ["null", "array"],
                  items: {
                    type: "object",
                    required: ["question", "options"],
                    properties: {
                      question: { type: "string" },
                      custom: { type: ["null", "boolean"] },
                      options: {
                        type: ["null", "array"],
                        items: {
                          type: "object",
                          required: ["label"],
                          properties: { label: { type: "string" } },
                        },
                      },
                    },
                  },
                },
              },
            },
          },
        ],
      },
    };
    const f = fakeFetch([
      { status: 200, contentType: "application/json", body: JSON.stringify(toolsList) },
    ]);
    const bridge = createBridge({
      remoteUrl: "http://example/mcp",
      getToken: async () => "tok",
      fetchImpl: f.impl,
    });

    const res = await bridge.handle({ jsonrpc: "2.0", id: 1, method: "tools/list" });
    type SchemaNode = {
      type?: unknown;
      items?: SchemaNode;
      properties?: Record<string, SchemaNode>;
    };
    const result = res?.result as { tools: Array<{ inputSchema: SchemaNode }> };
    const ask = result.tools[0]?.inputSchema.properties?.ask;

    // Nullable array collapses to a single-type array with items preserved.
    expect(ask?.type).toBe("array");
    expect(ask?.items).toBeDefined();
    // Nested nullable array (options) collapses too.
    expect(ask?.items?.properties?.options.type).toBe("array");
    expect(ask?.items?.properties?.options.items).toBeDefined();
    // Nullable boolean collapses to a single-type boolean.
    expect(ask?.items?.properties?.custom.type).toBe("boolean");
    // Nothing in the schema still uses a union type array (the shape Gemini rejects).
    expect(JSON.stringify(res)).not.toContain('["null"');
  });
});

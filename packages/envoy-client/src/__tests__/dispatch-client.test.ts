import { describe, expect, it } from "bun:test";
import type { DispatchServiceArguments } from "../dispatch-call";
import { callDispatch, DispatchServiceError } from "../dispatch-client";

interface MockResponse {
  status: number;
  statusText?: string;
  headers?: Record<string, string>;
  contentType?: string;
  body: string;
}

function fakeFetch(responses: MockResponse[]) {
  let index = 0;
  const calls: Array<{ url: string; init: RequestInit }> = [];
  const impl = (url: string, init?: RequestInit) => {
    calls.push({ url, init: init ?? {} });
    const next = responses[index++];
    if (!next) throw new Error(`no mock response for call #${index}`);
    return Promise.resolve(
      new Response(next.body, {
        status: next.status,
        statusText: next.statusText ?? "",
        headers: new Headers({
          "content-type": next.contentType ?? "application/json",
          ...(next.headers ?? {}),
        }),
      })
    );
  };
  return { impl: impl as unknown as typeof fetch, calls };
}

const args: DispatchServiceArguments = {
  subject: "s",
  context: "c",
  question: "q",
  repo: "acme-org/example-repo",
  origin: { host: "omp", cwd: "/home/ubuntu/legion", sessionId: "ses_1" },
};

const okResult = { thread: 12, url: "https://github.com/acme-org/example-repo/issues/12" };

function rpcResult(result: unknown): string {
  return JSON.stringify({
    jsonrpc: "2.0",
    id: 1,
    result: { content: [{ type: "text", text: JSON.stringify(result) }] },
  });
}

describe("callDispatch", () => {
  it("sends exactly one tools/call POST with this call's bearer and no session header", async () => {
    const f = fakeFetch([
      { status: 200, headers: { "mcp-session-id": "ignored" }, body: rpcResult(okResult) },
    ]);
    const result = await callDispatch(
      { serviceUrl: "http://example/mcp", getToken: async () => "tok-A", fetchImpl: f.impl },
      args
    );
    expect(result).toEqual(okResult);
    expect(f.calls).toHaveLength(1);
    const [call] = f.calls;
    expect(call?.url).toBe("http://example/mcp");
    expect(call?.init.method).toBe("POST");
    const headers = call?.init.headers as Record<string, string>;
    expect(headers.Authorization).toBe("Bearer tok-A");
    expect(headers.Accept).toBe("application/json, text/event-stream");
    expect(headers["Mcp-Session-Id"]).toBeUndefined();
    expect(JSON.parse(String(call?.init.body))).toEqual({
      jsonrpc: "2.0",
      id: 1,
      method: "tools/call",
      params: { name: "dispatch", arguments: args },
    });
  });

  it("mints a fresh token on every call and never reuses a session id", async () => {
    let issued = 0;
    const f = fakeFetch([
      { status: 200, headers: { "mcp-session-id": "S1" }, body: rpcResult(okResult) },
      { status: 200, body: rpcResult(okResult) },
    ]);
    const options = {
      serviceUrl: "http://example/mcp",
      getToken: async () => `tok-${++issued}`,
      fetchImpl: f.impl,
    };
    await callDispatch(options, args);
    await callDispatch(options, args);
    expect(issued).toBe(2);
    const second = f.calls[1]?.init.headers as Record<string, string>;
    expect(second.Authorization).toBe("Bearer tok-2");
    expect(second["Mcp-Session-Id"]).toBeUndefined();
  });

  it("parses a Streamable HTTP SSE response", async () => {
    const comment = "https://github.com/acme-org/example-repo/issues/12#issuecomment-5";
    const f = fakeFetch([
      {
        status: 200,
        contentType: "text/event-stream",
        body: `event: message\ndata: ${rpcResult({ ...okResult, comment })}\n\n`,
      },
    ]);
    const result = await callDispatch(
      { serviceUrl: "http://example/mcp", getToken: async () => "t", fetchImpl: f.impl },
      { ...args, subject: undefined, thread: "12" }
    );
    expect(result.comment).toBe(comment);
  });

  it("fails before any request when no token can be minted", async () => {
    const f = fakeFetch([]);
    const thrown = await callDispatch(
      { serviceUrl: "http://example/mcp", getToken: async () => null, fetchImpl: f.impl },
      args
    ).catch((error: unknown) => error);
    expect(thrown).toBeInstanceOf(DispatchServiceError);
    expect((thrown as DispatchServiceError).kind).toBe("auth");
    expect((thrown as Error).message).toBe(
      "dispatch: gh auth token returned empty in /home/ubuntu/legion — check your gh-app setup"
    );
    expect(f.calls).toHaveLength(0);
  });

  it("surfaces a tool error's text verbatim", async () => {
    const f = fakeFetch([
      {
        status: 200,
        body: JSON.stringify({
          jsonrpc: "2.0",
          id: 1,
          result: { isError: true, content: [{ type: "text", text: "#42 is closed; open a new thread" }] },
        }),
      },
    ]);
    const thrown = await callDispatch(
      { serviceUrl: "http://example/mcp", getToken: async () => "t", fetchImpl: f.impl },
      args
    ).catch((error: unknown) => error);
    expect((thrown as DispatchServiceError).kind).toBe("tool");
    expect((thrown as Error).message).toBe("#42 is closed; open a new thread");
  });

  it("surfaces a JSON-RPC error message", async () => {
    const f = fakeFetch([
      {
        status: 200,
        body: JSON.stringify({
          jsonrpc: "2.0",
          id: 1,
          error: { code: -32602, message: "invalid params" },
        }),
      },
    ]);
    const thrown = await callDispatch(
      { serviceUrl: "http://example/mcp", getToken: async () => "t", fetchImpl: f.impl },
      args
    ).catch((error: unknown) => error);
    expect((thrown as DispatchServiceError).kind).toBe("tool");
    expect((thrown as Error).message).toBe("invalid params");
  });

  it("reports 401 as an auth failure and other statuses as transport failures, without retrying", async () => {
    const unauthorized = fakeFetch([{ status: 401, body: '{"error":"missing bearer"}' }]);
    const a = await callDispatch(
      { serviceUrl: "http://example/mcp", getToken: async () => "t", fetchImpl: unauthorized.impl },
      args
    ).catch((error: unknown) => error);
    expect((a as DispatchServiceError).kind).toBe("auth");
    expect(unauthorized.calls).toHaveLength(1);
    const down = fakeFetch([{ status: 503, statusText: "Service Unavailable", body: "down" }]);
    const b = await callDispatch(
      { serviceUrl: "http://example/mcp", getToken: async () => "t", fetchImpl: down.impl },
      args
    ).catch((error: unknown) => error);
    expect((b as DispatchServiceError).kind).toBe("transport");
    expect((b as Error).message).toBe("dispatch service returned 503 Service Unavailable: down");
    expect(down.calls).toHaveLength(1);
  });

  it("reports a network failure as transport", async () => {
    const failing = (async () => {
      throw new Error("ECONNREFUSED");
    }) as unknown as typeof fetch;
    const thrown = await callDispatch(
      { serviceUrl: "http://example/mcp", getToken: async () => "t", fetchImpl: failing },
      args
    ).catch((error: unknown) => error);
    expect((thrown as DispatchServiceError).kind).toBe("transport");
    expect((thrown as Error).message).toBe(
      "dispatch service unreachable at http://example/mcp: ECONNREFUSED"
    );
  });
});

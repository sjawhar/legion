import { describe, expect, it } from "bun:test";
import type { DispatchOrigin } from "../dispatch-cwd";
import type { JsonRpcRequest } from "../dispatch-mcp-bridge";
import { type DispatchInjectionContext, injectDispatchArgs } from "../dispatch-mcp-shim";

const origin: DispatchOrigin = { host: "omp", machine: "example-host", cwd: "/home/ubuntu/legion" };

function dispatchCall(args: Record<string, unknown>, id: JsonRpcRequest["id"] = 1): JsonRpcRequest {
  return {
    jsonrpc: "2.0",
    id,
    method: "tools/call",
    params: { name: "dispatch", arguments: args },
  };
}

function context(repo: string | null): DispatchInjectionContext {
  return { repo, origin };
}

describe("injectDispatchArgs", () => {
  it("injects the resolved repo and origin when both are absent", () => {
    const result = injectDispatchArgs(
      dispatchCall({ subject: "s", context: "c", question: "q" }),
      context("acme/widgets")
    );
    expect(result.kind).toBe("forward");
    if (result.kind !== "forward") throw new Error("unreachable");
    const params = result.request.params as { arguments: Record<string, unknown> };
    expect(params.arguments.repo).toBe("acme/widgets");
    expect(params.arguments.origin).toEqual(origin);
  });

  it("replies with an isError tool result naming the cwd when no repo resolves", () => {
    const result = injectDispatchArgs(
      dispatchCall({ subject: "s", context: "c", question: "q" }),
      context(null)
    );
    expect(result.kind).toBe("reply");
    if (result.kind !== "reply") throw new Error("unreachable");
    expect(result.response).toEqual({
      jsonrpc: "2.0",
      id: 1,
      result: {
        isError: true,
        content: [
          {
            type: "text",
            text: "dispatch: /home/ubuntu/legion has no GitHub remote; pass repo=owner/name",
          },
        ],
      },
    });
  });

  it("leaves an explicit repo untouched even when a repo also resolves from cwd", () => {
    const result = injectDispatchArgs(
      dispatchCall({ subject: "s", context: "c", question: "q", repo: "explicit/repo" }),
      context("acme/widgets")
    );
    expect(result.kind).toBe("forward");
    if (result.kind !== "forward") throw new Error("unreachable");
    const params = result.request.params as { arguments: Record<string, unknown> };
    expect(params.arguments.repo).toBe("explicit/repo");
  });

  it("leaves an explicit repo untouched even when no repo resolves from cwd", () => {
    const result = injectDispatchArgs(
      dispatchCall({ subject: "s", context: "c", question: "q", repo: "explicit/repo" }),
      context(null)
    );
    expect(result.kind).toBe("forward");
    if (result.kind !== "forward") throw new Error("unreachable");
    const params = result.request.params as { arguments: Record<string, unknown> };
    expect(params.arguments.repo).toBe("explicit/repo");
  });

  it("skips repo injection for a fully-qualified parent, even with no cwd repo", () => {
    const result = injectDispatchArgs(
      dispatchCall({ subject: "s", context: "c", question: "q", parent: "acme/widgets#42" }),
      context(null)
    );
    expect(result.kind).toBe("forward");
    if (result.kind !== "forward") throw new Error("unreachable");
    const params = result.request.params as { arguments: Record<string, unknown> };
    expect(params.arguments.repo).toBeUndefined();
    expect(params.arguments.parent).toBe("acme/widgets#42");
    expect(params.arguments.origin).toEqual(origin);
  });

  it("skips repo injection for a qualified parent that also names a comment", () => {
    const result = injectDispatchArgs(
      dispatchCall({ subject: "s", context: "c", question: "q", parent: "acme/widgets#42#9001" }),
      context(null)
    );
    expect(result.kind).toBe("forward");
    if (result.kind !== "forward") throw new Error("unreachable");
    const params = result.request.params as { arguments: Record<string, unknown> };
    expect(params.arguments.repo).toBeUndefined();
    expect(params.arguments.parent).toBe("acme/widgets#42#9001");
  });

  it("treats a bare parent with a comment id as unqualified and injects repo", () => {
    const result = injectDispatchArgs(
      dispatchCall({ subject: "s", context: "c", question: "q", parent: "42#9001" }),
      context("acme/widgets")
    );
    expect(result.kind).toBe("forward");
    if (result.kind !== "forward") throw new Error("unreachable");
    const params = result.request.params as { arguments: Record<string, unknown> };
    expect(params.arguments.repo).toBe("acme/widgets");
  });

  it("still injects repo for a bare-number parent", () => {
    const result = injectDispatchArgs(
      dispatchCall({ subject: "s", context: "c", question: "q", parent: "42" }),
      context("acme/widgets")
    );
    expect(result.kind).toBe("forward");
    if (result.kind !== "forward") throw new Error("unreachable");
    const params = result.request.params as { arguments: Record<string, unknown> };
    expect(params.arguments.repo).toBe("acme/widgets");
  });

  it("leaves an explicit origin untouched", () => {
    const explicitOrigin = { cwd: "/somewhere/else" };
    const result = injectDispatchArgs(
      dispatchCall({ subject: "s", context: "c", question: "q", origin: explicitOrigin }),
      context("acme/widgets")
    );
    expect(result.kind).toBe("forward");
    if (result.kind !== "forward") throw new Error("unreachable");
    const params = result.request.params as { arguments: Record<string, unknown> };
    expect(params.arguments.origin).toBe(explicitOrigin);
  });

  it("passes non-dispatch tool calls through untouched", () => {
    const request: JsonRpcRequest = {
      jsonrpc: "2.0",
      id: 1,
      method: "tools/call",
      params: { name: "some_other_tool", arguments: { repo: "should/not-matter" } },
    };
    const result = injectDispatchArgs(request, context(null));
    expect(result).toEqual({ kind: "forward", request });
  });

  it("passes non-tools/call requests through untouched", () => {
    const request: JsonRpcRequest = { jsonrpc: "2.0", id: 1, method: "tools/list" };
    const result = injectDispatchArgs(request, context(null));
    expect(result).toEqual({ kind: "forward", request });
  });

  it("passes notifications through untouched", () => {
    const request: JsonRpcRequest = { jsonrpc: "2.0", method: "notifications/initialized" };
    const result = injectDispatchArgs(request, context(null));
    expect(result).toEqual({ kind: "forward", request });
  });
});

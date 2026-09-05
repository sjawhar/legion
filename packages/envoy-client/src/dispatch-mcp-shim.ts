// Shared runner for the local dispatch MCP shim.
//
// Coding-agent hosts spawn this as a stdio MCP server. It reads newline-delimited
// JSON-RPC messages from stdin, forwards each to the remote dispatch server's
// Streamable HTTP /mcp endpoint, and writes responses to stdout.
//
// The bridge obtains GitHub bearers from the user's `gh` shim, refreshes them
// before expiry, and retries once immediately after a 401 response.
//
// Before forwarding a `dispatch` tool call, the shim fills in `repo` and
// `origin` from the session's cwd — the one thing every host (OMP, OpenCode,
// Claude) shares — so the remote server never has to guess at either.

import * as readline from "node:readline";
import { resolveDispatchConfig } from "./dispatch-config";
import { type DispatchOrigin, defaultExec, resolveCwdRepo, resolveOrigin } from "./dispatch-cwd";
import {
  createBridge,
  defaultGhTokenGetter,
  type JsonRpcRequest,
  type JsonRpcResponse,
} from "./dispatch-mcp-bridge";
import { messageFor } from "./errors";

/**
 * Narrow one stdin line to a JSON-RPC request. The bridge relays the parsed
 * object verbatim, so the line is checked rather than rebuilt — rebuilding
 * would drop fields the remote server expects. `id` is absent or null for
 * notifications, which the bridge answers with no response.
 */
export function isJsonRpcRequest(value: unknown): value is JsonRpcRequest {
  if (typeof value !== "object" || value === null) return false;
  if (!("jsonrpc" in value) || value.jsonrpc !== "2.0") return false;
  if (!("method" in value) || typeof value.method !== "string") return false;
  if (!("id" in value)) return true;
  return value.id === null || typeof value.id === "string" || typeof value.id === "number";
}

export type DispatchShimGate =
  | { readonly serve: false; readonly reason: string }
  | { readonly serve: true; readonly remoteUrl: string };

/**
 * Decide whether this process may serve the dispatch tool.
 *
 * Every session gets it when dispatch is enabled — including Legion sessions:
 * dispatch exists so headless unattended agents (architects, planners, phase
 * workers) can raise durable questions to the human, and the asking session
 * receives the reply. When an envoy.json is present but invalid, the reason
 * names the file and key so the fix is obvious instead of looking identical
 * to dispatch simply being turned off.
 */
export function dispatchShimGate(
  env: Record<string, string | undefined>,
  options: { readonly cwd?: string; readonly home?: string } = {}
): DispatchShimGate {
  const { url: remoteUrl, error } = resolveDispatchConfig(env, options);
  if (remoteUrl === null) {
    return {
      serve: false,
      reason:
        error ?? "dispatch is not enabled — set dispatch.enabled in envoy.json or DISPATCH_MCP_URL",
    };
  }
  return { serve: true, remoteUrl };
}

/**
 * A `tools/call` params object already narrowed to the `dispatch` tool. The
 * index signature keeps every other key the host sent, so the request can be
 * relayed verbatim except for the arguments the shim fills in.
 */
interface DispatchToolCallParams {
  readonly name: "dispatch";
  readonly arguments?: Record<string, unknown>;
  readonly [key: string]: unknown;
}

/** A fully-qualified dispatch parent: "owner/name#123" or "owner/name#123#456". */
const QUALIFIED_PARENT_RE = /^[^/\s#]+\/[^/\s#]+#\d+/;

/** The cwd-derived defaults the shim has available to fill into a `dispatch` call. */
export interface DispatchInjectionContext {
  readonly repo: string | null;
  readonly origin: DispatchOrigin;
}

export type InjectDispatchArgsResult =
  | { readonly kind: "forward"; readonly request: JsonRpcRequest }
  | { readonly kind: "reply"; readonly response: JsonRpcResponse };

/**
 * The params of a `tools/call` for `dispatch` whose `arguments` the shim can
 * fill in; null for every other request, including a `dispatch` call whose
 * `arguments` is not an object (the server rejects it as-is).
 */
function dispatchCallParams(request: JsonRpcRequest): DispatchToolCallParams | null {
  if (request.method !== "tools/call") return null;
  return isDispatchToolCallParams(request.params) ? request.params : null;
}

function isDispatchToolCallParams(params: unknown): params is DispatchToolCallParams {
  if (typeof params !== "object" || params === null) return false;
  if (!("name" in params) || params.name !== "dispatch") return false;
  const args = "arguments" in params ? params.arguments : undefined;
  return args === undefined || (typeof args === "object" && args !== null && !Array.isArray(args));
}

/**
 * Fill a `dispatch` tool call's `repo` and `origin` arguments from the
 * session's cwd before it reaches the remote server. Every other request —
 * including calls to other tools — passes through untouched, as does any
 * `dispatch` call that already names an explicit `repo` or a fully-qualified
 * `parent`. Pure: `context` is already resolved by the caller.
 */
export function injectDispatchArgs(
  request: JsonRpcRequest,
  context: DispatchInjectionContext
): InjectDispatchArgsResult {
  const params = dispatchCallParams(request);
  if (params === null) return { kind: "forward", request };

  const args = { ...params.arguments };
  const parent = typeof args["parent"] === "string" ? args["parent"] : undefined;
  const hasQualifiedParent = parent !== undefined && QUALIFIED_PARENT_RE.test(parent);

  if (args["repo"] === undefined && !hasQualifiedParent) {
    if (context.repo === null) {
      return {
        kind: "reply",
        response: {
          jsonrpc: "2.0",
          id: request.id ?? null,
          result: {
            isError: true,
            content: [
              {
                type: "text",
                text: `dispatch: ${context.origin.cwd} has no GitHub remote; pass repo=owner/name`,
              },
            ],
          },
        },
      };
    }
    args["repo"] = context.repo;
  }

  if (args["origin"] === undefined) {
    args["origin"] = context.origin;
  }

  return {
    kind: "forward",
    request: { ...request, params: { ...params, arguments: args } },
  };
}

export function runDispatchMcpShim(): void {
  const gate = dispatchShimGate(process.env);
  if (!gate.serve) {
    process.stderr.write(`envoy-dispatch shim: ${gate.reason}\n`);
    process.exit(0);
  }

  const bridge = createBridge({
    remoteUrl: gate.remoteUrl,
    getToken: defaultGhTokenGetter,
  });

  // Resolved lazily (only once a `dispatch` call actually arrives, since
  // most tool calls never need it) and cached for the process lifetime —
  // the cwd and its GitHub remote don't change during a session.
  let context: Promise<DispatchInjectionContext> | null = null;
  function dispatchContext(): Promise<DispatchInjectionContext> {
    if (context === null) {
      const cwd = process.cwd();
      context = (async () => ({
        repo: await resolveCwdRepo(cwd, defaultExec),
        origin: await resolveOrigin(process.env, defaultExec, cwd),
      }))();
    }
    return context;
  }

  async function handle(request: JsonRpcRequest): Promise<JsonRpcResponse | null> {
    if (dispatchCallParams(request) === null) {
      return bridge.handle(request);
    }
    const injected = injectDispatchArgs(request, await dispatchContext());
    return injected.kind === "reply" ? injected.response : bridge.handle(injected.request);
  }

  const rl = readline.createInterface({ input: process.stdin });

  let inflight = 0;
  let closed = false;

  // Serialize incoming requests. MCP requires the initialize handshake to
  // complete before any tool calls are processed; running rl.on("line")
  // callbacks in parallel would race tools/call against initialize and hit
  // `invalid during session initialization` from the server. Even after
  // init, sequencing keeps the wire ordering deterministic, which is what
  // stdio MCP hosts expect.
  let chain: Promise<void> = Promise.resolve();

  function maybeExit(): void {
    if (closed && inflight === 0) process.exit(0);
  }

  rl.on("line", (line) => {
    const trimmed = line.trim();
    if (!trimmed) return;
    inflight++;
    chain = chain.then(async () => {
      try {
        const parsed: unknown = JSON.parse(trimmed);
        if (!isJsonRpcRequest(parsed)) {
          throw new TypeError(`not a JSON-RPC 2.0 request: ${trimmed.slice(0, 120)}`);
        }
        const response = await handle(parsed);
        if (response !== null) {
          process.stdout.write(`${JSON.stringify(response)}\n`);
        }
      } catch (err) {
        process.stderr.write(`envoy-dispatch shim: ${messageFor(err)}\n`);
      } finally {
        inflight--;
        maybeExit();
      }
    });
  });

  rl.on("close", () => {
    closed = true;
    maybeExit();
  });

  process.on("SIGTERM", () => process.exit(0));
  process.on("SIGINT", () => process.exit(0));
}

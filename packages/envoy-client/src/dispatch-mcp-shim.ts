// Shared runner for the local dispatch MCP shim.
//
// Coding-agent hosts spawn this as a stdio MCP server. It reads newline-delimited
// JSON-RPC messages from stdin, forwards each to the remote dispatch server's
// Streamable HTTP /mcp endpoint, and writes responses to stdout.
//
// The bridge obtains GitHub bearers from the user's `gh` shim, refreshes them
// before expiry, and retries once immediately after a 401 response.

import * as readline from "node:readline";
import { resolveDispatchMcpUrl } from "./dispatch-config";
import { createBridge, defaultGhTokenGetter, type JsonRpcRequest } from "./dispatch-mcp-bridge";
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
 * receives the reply.
 */
export function dispatchShimGate(
  env: Record<string, string | undefined>,
  options: { readonly cwd?: string; readonly home?: string } = {}
): DispatchShimGate {
  const remoteUrl = resolveDispatchMcpUrl(env, options);
  if (remoteUrl === null) {
    return {
      serve: false,
      reason: "dispatch is not enabled — set dispatch.enabled in envoy.json or DISPATCH_MCP_URL",
    };
  }
  return { serve: true, remoteUrl };
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
        const response = await bridge.handle(parsed);
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

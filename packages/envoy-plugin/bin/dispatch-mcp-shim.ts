#!/usr/bin/env bun
// envoy-plugin local MCP shim.
//
// Spawned by OpenCode as a `type: "local"` MCP transport. Reads JSON-RPC
// messages from stdin (newline-delimited), forwards each to the remote
// dispatch server's Streamable HTTP /mcp endpoint with a fresh GitHub
// bearer minted via the user's `gh` shim, and writes responses to stdout.
//
// Token rotation is invisible to OpenCode — the shim handles 50-minute
// refresh cycles + immediate retry on 401. This avoids the "MCP dies
// after 1 hour" failure mode of static-header configurations.

import * as readline from "node:readline";
import {
  createBridge,
  defaultGhTokenGetter,
  type JsonRpcRequest,
} from "../src/dispatch-mcp-bridge";

const remoteUrl = process.env.DISPATCH_MCP_URL;
if (!remoteUrl) {
  process.stderr.write("envoy-dispatch shim: DISPATCH_MCP_URL is required\n");
  process.exit(1);
}

const bridge = createBridge({
  remoteUrl,
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
// OpenCode expects for stdio MCP transports.
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
      const request = JSON.parse(trimmed) as JsonRpcRequest;
      const response = await bridge.handle(request);
      if (response !== null) {
        process.stdout.write(`${JSON.stringify(response)}\n`);
      }
    } catch (err) {
      const msg = err instanceof Error ? err.message : String(err);
      process.stderr.write(`envoy-dispatch shim: ${msg}\n`);
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

#!/usr/bin/env bun
// envoy-plugin local MCP shim entry point. The forwarding logic is shared
// with the other Envoy adapters in @legion/envoy-client; OpenCode spawns
// this wrapper via the `mcp.envoy` entry injected by src/dispatch-mcp.ts.
import { runDispatchMcpShim } from "@legion/envoy-client/dispatch-mcp-shim";

runDispatchMcpShim();

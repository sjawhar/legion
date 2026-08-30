#!/usr/bin/env bun
// Dispatch MCP shim entry point. Mount as a stdio MCP server with
// DISPATCH_MCP_URL pointing at the dispatch server's /mcp endpoint.
import { runDispatchMcpShim } from "../src/dispatch-mcp-shim";

runDispatchMcpShim();

// Claude Code dispatch MCP shim entry point. The plugin's .mcp.json mounts
// this for every Claude session; the forwarding logic is shared with the OMP
// and OpenCode adapters. The shim self-gates on envoy.json `dispatch.enabled`,
// so sessions on machines without dispatch configured mount nothing.
import { runDispatchMcpShim } from "@legion/envoy-client/dispatch-mcp-shim"

runDispatchMcpShim()

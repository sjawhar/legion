#!/usr/bin/env bun
// pi-envoy local MCP shim entry point. OMP spawns this via the package-root
// .mcp.json mount (see docs/solutions/envoy/omp-extension-mcp-mounting.md);
// the forwarding logic is shared with the other Envoy adapters. The shim
// self-gates on envoy.json `dispatch.enabled`, so sessions on machines
// without dispatch configured mount nothing.
import { runDispatchMcpShim } from "@legion/envoy-client/dispatch-mcp-shim";

runDispatchMcpShim();

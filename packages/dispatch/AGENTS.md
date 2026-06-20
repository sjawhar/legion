# Dispatch Package

The Dispatch dashboard SPA — the human approval and coordination surface used
by Legion and Envoy workflows.

## Overview

This package now contains only the Vite SPA. The HTTP backend has been moved
to a Go binary in the `envoy` package:

| Layer                | Location                                                |
| -------------------- | ------------------------------------------------------- |
| Dashboard SPA (this) | `packages/dispatch/web/`                                |
| Backend HTTP server  | `packages/envoy/cmd/dispatch/`                          |
| Backend internals    | `packages/envoy/internal/dispatch/`                     |
| MCP endpoint         | `packages/envoy/internal/dispatch/mcp/` (served at `/mcp`) |

The Go server serves the SPA build artifacts from `packages/dispatch/web/dist/`
and exposes the OAuth + GitHub proxy + SSE + MCP routes.

## Local development

```bash
# Build the SPA
bun run build:web

# Start the backend (from packages/envoy)
go run ./cmd/dispatch
```

Use `bun run typecheck`, `bun run lint`, and `bun test` before reporting SPA
changes. For backend changes see `packages/envoy/cmd/dispatch/AGENTS.md`.

## Configuration

Dispatch reads the same `envoy.json` shape as the Envoy plugin.

- User config: `~/.config/opencode/envoy.json`
- Repo config: `<repo>/.opencode/envoy.json`

Repo config overrides user config. The top-level object is shallow-merged, and
the `dispatch` sub-object is shallow-merged so repo `dispatch` keys override
only the matching user `dispatch` keys.

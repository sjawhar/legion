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

## Discovery

The dashboard has no watched-repos configuration. On sign-in it fetches the
signed-in user's Envoy App installations (`GET /api/installations`) and
searches `is:issue is:open label:dispatch-thread` scoped to `user:<owner>`
for every distinct installation owner (user or org) the App is installed on
and the signer can see. A new repo under an already-installed owner needs no
configuration; a user with zero visible installations sees an explicit error
state instead of an empty sidebar.

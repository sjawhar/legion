# Dispatch Package

The Dispatch dashboard SPA — the human approval and coordination surface used
by Legion and Envoy workflows.

## Overview

This package contains only the Vite SPA. The HTTP backend is a Go binary in
the `envoy` package:

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

## Conversations

A thread's questions come from the issue body and from every `dispatch:ask` follow-up
comment (`web/src/asks.ts`); an ask is open until an answer comment names its `askId`
(a legacy answer without `forAsk` settles the body's asks by index). The detail view renders
one form per open ask (`#detail-ask-forms`) and each answer beneath the question it settles;
the sidebar's `needs you` badge counts open asks from the last 30 comments returned by the
search query, or from the full comment list once loaded.

Painting never rebuilds the page: `web/src/main.ts` patches regions by id and `web/src/dom.ts`
reconciles ask forms by `askId`, so the reply textarea and half-filled forms survive events.
GitHub references in rendered markdown are linkified and unfurled to titles through the REST
proxy (`web/src/unfurl.ts`). Browser behaviour is covered by `bun run e2e`
(`e2e/`, Playwright against a fixture backend that speaks the service's HTTP contract).

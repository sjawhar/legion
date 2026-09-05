---
title: "Go MCP SDK handler context is the initialize request's — per-call auth must read req.Extra.Header, and a client that never re-initializes needs a stateless server"
category: envoy
tags:
  - dispatch
  - mcp
  - go-sdk
  - streamable-http
  - authentication
  - envoy-client
date: 2026-09-05
status: active
module: envoy
problem_type: logic_error
severity: high
symptoms:
  - "dispatch tool returns `401 Bad credentials` on the dedupe search for every attempt, for hours, while the same token the shim mints returns 200 against the same GitHub endpoint when sent directly"
  - "Killing the dispatch-mcp-shim processes (so OMP respawns them) fixes it instantly; nothing else does"
  - "Only sessions whose shim is older than ~1 hour fail; fresh sessions dispatch fine"
  - "After a dispatch server restart, every existing shim returns `remote 404 Not Found session not found` on every call"
root_cause: "The dispatch MCP server read the bearer from the tool handler's ctx. In go-sdk's stateful Streamable HTTP mode (the default), server.Connect(req.Context(), …) runs once per session, at initialize, and every later handler ctx derives from that connection, so the session was pinned to the token it was initialized with. The shim keeps one MCP session per process and mints a fresh GitHub installation token per call; installation tokens expire in ≤60 min. Separately, stateful mode validated Mcp-Session-Id against in-memory sessions, and the shim never re-initializes."
resolution_type: code_fix
resolution: "Read the bearer from req.Extra.Header, which go-sdk fills from each POST's headers; serve the endpoint with StreamableHTTPOptions{Stateless: true}. Regression tests drive real Streamable HTTP sessions through the go-sdk client."
---

## Problem

An agent's `dispatch` calls failed with `401 Bad credentials` for ~9 hours. The shim's token
cache, TTL, and 401-retry were tested and correct, and the token it was minting was valid.
Every explanation below the shim was ruled out with evidence before the actual boundary —
shim → Go MCP server — was read.

## Symptoms

- `search issues: GET https://api.github.com/search/issues?…: 401 Bad credentials []` from
  the dispatch tool, persisting across the shim's 50-minute cache TTL and its forced
  re-mint on 401.
- The token the shim had just minted, sent directly with curl, returned 200.
- Restarting the shim process fixed it. Waiting did not.

## What Didn't Work

- **Suspecting the shim's TTL / 401-retry** (`packages/envoy-client/src/dispatch-mcp-bridge.ts`).
  Simulated tests against the real `createBridge` (injectable `getToken`/`fetchImpl`/`now`)
  passed: it re-mints on 401 and sends the new token on the second attempt. A first draft of
  one test "failed" because the fake server only whitelisted the first token — a harness bug
  that briefly looked like the reported defect.
- **Suspecting the credential source beneath the shim.** Repeated mints from the shim's own
  environment returned a valid token; nothing there explained a failure that persisted after
  the token had been refreshed.

Each of these was a plausible story that "restart fixed it" was equally consistent with. Only
reading the shim→server boundary produced a mechanism that explained *all* observations,
including a failure that persisted after the credential beneath the shim had been refreshed.

## Solution

`packages/envoy/internal/dispatch/mcp/server.go`:

```go
// before — ctx descends from the initialize request; this is the FIRST bearer forever
token := bearerFromContext(ctx)

// after — the headers of the HTTP request carrying this specific tools/call
token := extractBearer(req.Extra.Header)
```

```go
// before — stateful: Mcp-Session-Id validated against in-memory sessions
mcpsdk.NewStreamableHTTPHandler(getServer, nil)

// after — stateless: unknown session ids are accepted; a temporary session per POST
mcpsdk.NewStreamableHTTPHandler(getServer, &mcpsdk.StreamableHTTPOptions{Stateless: true})
```

The middleware still rejects bearer-less requests with 401 at the HTTP layer; it just no
longer tries to smuggle the token through the context.

## Why This Works

go-sdk v1.6.1, `mcp/streamable.go`:

- `:494` `server.Connect(req.Context(), transport, connectOpts)` runs inside the
  `if sessInfo == nil` block. In stateful mode that is once per session, at `initialize`;
  `ServerSession.handle` (`mcp/server.go:1451`) builds every handler ctx from that connection
  and adds only the request id, so middleware context values from later POSTs never reach a
  handler. (In stateless mode `sessInfo` is nil for every POST, so `Connect` — and therefore
  a ctx-derived bearer — would coincidentally be per-call. That is why the header contract
  is pinned at the handler level below, not left to the transport mode.)
- `:1169–1172` `jreq.Extra = &RequestExtra{TokenInfo, Header: req.Header}` is set for every
  message of every POST. `req.Extra.Header` is the supported per-request surface.
- `:301` in stateless mode an unknown `Mcp-Session-Id` is not rejected; `:426–472` a
  temporary session with default initialized state is built per request and closed after.
  Every client — today the `dispatch` tool inside each host plugin, via
  `packages/envoy-client/src/dispatch-client.ts` — sends one `tools/call` POST per
  invocation with a token minted for that call and holds no session, which is all stateless
  mode gives up.

Proven against the production container before the fix: initialize with a garbage bearer +
call with a valid one → 401 (the garbage init token was used); initialize valid + call
garbage → 422 for a nonexistent repo (the call token was ignored). After the fix both flip,
and a `tools/call` carrying a session id the server has never seen is served rather than 404.

## Prevention

- **Per-request auth in a go-sdk MCP handler reads `req.Extra.Header`, never `ctx`.** In
  stateful mode a context-value middleware only ever reaches the handler with the
  `initialize` POST's values; in stateless mode it happens to work, which is the more
  dangerous case because a maintainer will try ctx, see it pass, and not know why the header
  path is mandated. `TestDispatchHandlerReadsTheBearerFromTheCallsOwnHeaders` calls the
  handler directly with a bearer only in `req.Extra.Header` and fails on a ctx-derived
  implementation regardless of transport mode; `TestDispatchUsesTheBearerOfEachCall` rotates
  the bearer between initialize and tools/call through the real go-sdk client against the
  shipped configuration.
- **A client that holds one session id for its process lifetime needs a server that does not
  validate session ids.** `TestDispatchServesSessionsFromBeforeARestart` POSTs a tools/call
  carrying a session id the server has never seen and asserts it is served, not 404.
- **Test the boundary, not the component you suspect.** The shim tests were correct and
  irrelevant. When "restart fixed it" is the only evidence, enumerate every component a
  restart resets (here: the shim's in-memory cache *and* the server-side session) and probe
  each with a request that distinguishes them — the garbage-init/valid-call pair did in one
  curl.

## Related

- `docs/solutions/envoy/dispatch-thread-provenance.md` — why the calling plugin, not the
  server, owns session context; the same "server stays stateless" decision this fix completes.
- `docs/superpowers/specs/2026-09-05-dispatch-conversations-design.md` §7 — the protocol
  note that records the one-stateless-request-per-call contract so it is not reintroduced.

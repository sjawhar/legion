---
title: "SSE test mocks: set shared state before writing trigger events"
category: testing
tags:
  - sse
  - race-condition
  - test-mock
  - streaming
  - envoy
  - flaky-test
date: 2026-04-11
status: active
module: envoy
related_issues:
  - "373"
symptoms:
  - "timeout waiting for response to initialize"
  - "TestHTTPServer_StartAndStop flaky on CI"
  - "sendSSE silently drops response"
  - "test passes locally but fails in CI"
---

# SSE Test Mocks: Set Shared State Before Writing Trigger Events

## Context

Envoy's MCP bridge uses HTTP/SSE transport where a mock server writes an "endpoint" event
to the SSE stream, signaling the client to start sending JSON-RPC requests. The test mock
(`mockHTTPMCPServer`) had a race condition that only manifested on slow CI runners.

## The Bug

The mock's `/sse` handler set `sseWriter`/`sseConn` fields AFTER writing the endpoint event:

```go
// WRONG: signal before state
fmt.Fprintf(w, "event: endpoint\ndata: %s\n\n", messageURL)
flusher.Flush()

m.mu.Lock()
m.sseWriter = flusher  // Too late — client may have already sent a request
m.sseConn = w
m.mu.Unlock()
```

On slow CI, the client read the endpoint event and immediately POSTed an `initialize`
request. The `/message` handler called `sendSSE()`, which found nil fields and silently
returned. The `initialize` response was never sent → 30-second timeout.

## The Fix

Set shared state BEFORE writing the trigger event:

```go
// CORRECT: state before signal
m.mu.Lock()
m.sseWriter = flusher
m.sseConn = w
m.mu.Unlock()

fmt.Fprintf(w, "event: endpoint\ndata: %s\n\n", messageURL)
flusher.Flush()
```

## Pattern: Signal-Before-State Anti-Pattern

This is the streaming equivalent of publishing a service endpoint before the service is
listening. The trigger event is a contract: "I'm ready to accept requests." All state
backing that contract must be established before the signal is emitted.

**Checklist for streaming test mocks:**

1. **Set all shared fields before writing the readiness event.** The readiness event
   (first SSE event, WebSocket open frame, etc.) triggers client action. Fields must
   be ready before that trigger.

2. **Never silently drop operations in test helpers.** The `sendSSE()` method returned
   silently on nil fields, making the race invisible. In test mocks, prefer logging or
   failing explicitly so races surface immediately.

3. **Races only manifest under load.** Locally, goroutine scheduling is fast enough
   that the handler sets fields before the client acts. CI runners with higher contention
   expose the window. Run `-count=10` or `-race` to catch these locally.

## Scope

This pattern applies specifically to test mocks for streaming protocols (SSE, WebSocket,
gRPC streams) where the mock's handler goroutine sets shared state that callback methods
depend on. Synchronous test setups (where all fields are set before the test starts) are
not affected.

As of 2026-04-11, the `mockHTTPMCPServer` in `packages/envoy/internal/mcpbridge/` was
the only streaming mock in the envoy package with this pattern.

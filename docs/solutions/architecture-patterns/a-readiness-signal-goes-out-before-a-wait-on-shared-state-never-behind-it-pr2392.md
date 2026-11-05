---
title: "A readiness signal goes out before a wait on shared state, never behind it"
category: architecture-patterns
tags:
  - readiness
  - streaming
  - postgres
  - shutdown
date: 2026-10-30
status: active
module: packages/envoy/internal/dispatch/api
related_issues:
  - "sjawhar/legion#2392"
---

# A readiness signal goes out before a wait on shared state, never behind it

- When a handler, a hub or a worker must wait on state it does not own (an instance-wide snapshot, another process, an external service), send its readiness signal first: write the response headers and a heartbeat, close the `ready` channel, answer the handshake. Then run the wait on a goroutine of its own.
- Gate readiness only on the state the component owns. Seed what depends on the shared state when that wait ends. Then mark every view the seed could change as dirty, so nothing that arrived meanwhile is lost.
- Bound the wait by the request's or the lifetime's context, so shutdown and `httptest.Server.Close` never block on it.

## Evidence

In sjawhar/legion#2392, `Horizon.Await` (`packages/envoy/internal/dispatch/events/horizon.go`) waits until every transaction open on the Postgres instance has ended. Its first callers ran it before the SSE handler wrote any header and before the delivery hub closed `ready`. So a single `begin; select pg_current_xact_id(); select pg_sleep(20)` in another database on the same instance stopped the process from serving anything live. It also left `httptest.Server.Close` blocked, and `envoy-go` reported a 10-minute package timeout rather than a named test.

Commit `8c2e6451627e` fixed the stream: a resuming stream opens and heartbeats at once, and its subscription runs on a goroutine. Commit `eadaba021cbf` fixed the hub: `ready` waits only for the delivery-change cursor, which the hub owns, and the event cursor is seeded once the horizon is proven, then every view is marked dirty.

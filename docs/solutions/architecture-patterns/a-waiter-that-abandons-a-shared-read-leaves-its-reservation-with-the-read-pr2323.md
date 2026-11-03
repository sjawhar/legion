---
title: "A waiter that abandons a shared read leaves its reservation with the read"
category: architecture-patterns
tags:
  - singleflight
  - semaphore
  - backpressure
  - context
date: 2026-10-28
status: active
module: packages/envoy
---

# A waiter that abandons a shared read leaves its reservation with the read

Extends docs/solutions/architecture-patterns/a-deadline-that-abandons-without-cancelling-makes-a-serial-queue-concurrent-INFRA-246.md.

- When requests share one read (`singleflight.DoChan` on `context.WithoutCancel`, so one client
  leaving fails none of the others), a request that stops waiting must not release its semaphore
  reservation on return. Hand the reservation to the read: wait on the flight's result channel in
  a goroutine and release there, and zero the request's own deferred release.
- Without that, a client that cancels each request the moment it reaches the store starts any
  number of detached reads at once, and the cap bounds nothing.
- Test it with a store that blocks until released and records its peak concurrency: send more
  abandoned requests than the cap has room for, assert the peak stays at the room, then release
  and assert a fresh request can take the whole cap.

## Evidence

Pull request #2323, `packages/envoy/internal/dispatch/routes/router.go:599-604` (the handoff) and
`:643` (the detached read); `TestStaticHandlerHoldsTheReservationOfAnAbandonedRequestUntilItsReadEnds`
(`assets_test.go:743`). Before `801a4f26`: `20 abandoned requests held 20 store reads at once,
want at most the cap's room of 8`.

---
title: "A stop budget runs from the signal, and shutdown waits for handlers, not for an idle pool"
category: envoy
tags:
  - shutdown
  - context
  - graceful-stop
  - dispatch
date: 2026-10-29
status: active
module: packages/envoy/cmd/dispatch
---

# A stop budget runs from the signal, and shutdown waits for handlers, not for an idle pool

Extends docs/solutions/daemon/a-background-task-the-shim-owns-ends-on-the-stop-signal-and-is-waited-for-inside-the-pods-real-stop-grace-LEGION-629.md.

- Arm a background worker's stop deadline when its run context ends, before any setup that can
  block (a bind, a lookup): `context.AfterFunc(ctx, …)` over `context.WithoutCancel(ctx)`. Every
  later step (the bind still in flight, the wait for handlers, the drain) then spends that one
  budget; none starts a budget of its own.
- Before closing a pool, wait for every background handler that writes through it. An idle pool
  does not mean no handler is running: a handler blocked on an outside call (GitHub, a model
  service) holds no connection. Pass the worker's wait into the shutdown sequence beside the other
  waits, before the pool closes.
- A deferred stop on a path that can exit without the signal (a panic, an early return) cancels
  the context before it waits. Defers run last in, first out, so a `defer cancel()` declared
  earlier runs after the wait and the process hangs with the panic unprinted.
- Prove each window on the real binary: SIGTERM while an outside call is held, SIGTERM after the
  worker's connection was replaced, SIGTERM while it stands by on a bind poll, and a panic after it
  started.

## Evidence

Pull request sjawhar/legion#2391, the GitHub intake's stop. Three review findings in two rounds:

- At 4f9bd6e8 Dispatch closed its pool 0.6 s after SIGTERM on an idle server while an intake
  handler waited on GitHub; the handler then logged `closed pool` and acknowledged a message it
  never wrote. Fixed in 823f782f (`waitIntakes` in `cmd/dispatch/shutdown.go`);
  `TestShutdownWaitsForAnIntakeHandlerBeforeClosingThePool`.
- That fix's `defer waitIntakes()` waited before main's earlier `defer cancel()` ran, so a panic
  during startup hung. Fixed in a56be739 (`stopIntakesOnExit` in `cmd/dispatch/main.go`);
  `TestAPanicAfterTheIntakesStartEndsTheProcess` hung 20 s without it.
- The stop budget was created after the bind returned, so a bind finishing after the signal moved
  the deadline. Fixed in a56be739 (`stopContext` in
  `internal/dispatch/delivery/intake_delivery.go`);
  `TestABindThatReturnsAfterTheSignalStopsByTheBudgetFromTheSignal` returned 5.0 s after the signal
  with a 3 s budget on the old code.

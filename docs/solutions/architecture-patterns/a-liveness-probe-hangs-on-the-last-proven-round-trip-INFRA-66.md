---
title: "A liveness probe hangs on the last proven round trip, and that clock lives with the connection"
category: architecture-patterns
tags:
  - liveness-probe
  - postgres-listen
  - shared-component
  - multiprocess
date: 2026-10-23
status: active
module: packages/envoy/internal/dispatch/pglisten
related_issues:
  - "INFRA-66"
  - "sjawhar/legion#2087"
---

# A liveness probe hangs on the last proven round trip, and that clock lives with the connection

Extends docs/solutions/architecture-patterns/a-publish-whose-acknowledgement-comes-from-someone-other-than-the-receiver-INFRA-113.md.
That document says a receipt from someone other than the receiver proves nothing about the
transport. The delta here is the clock a probe is due on, and who may move it.

- Keep the probe's due time as `now − lastProvenRoundTrip`. Only an event that crossed the wire
  sets that stamp: the connect handshake, a message received, a successful probe.
- Nothing a caller schedules — a wake, a tick, a deadline it asked for — may set that stamp or
  stand in for it. A caller's requested deadline may only be **capped** at the probe's due time.
- A bound on a caller's own scheduling is a *rate* bound, never a liveness guarantee. When the
  caller's next-wake is a **minimum over N items**, bounding each item's re-arm bounds nothing:
  N items staggered inside one idle period hold every wait under it for ever.
- An optional hook added to a shared component makes every partial combination a reachable
  configuration. Enumerate them, decide each deliberately, test them. Where one caller's
  misconfiguration takes every other caller down with it — one goroutine, one process — prefer
  the silent-safe reading, then disclose that it is findable only by its silence.
- Prove a new seam **inert** for the callers that do not use it by running their own tests, not
  by reading the branch.

## Evidence

`pglisten.Listener` is one LISTEN connection with a redial loop and an idle ping, shared by three
callers. The ping is the only detector of a half-open connection: `WaitForNotification` on such a
socket returns deadline-exceeded rather than an error, and pgx keeps the connection open.

Three designs let a caller's timer stand in for the round trip.

1. A caller gained a wake (the stale-room evictor's deferred re-examination, which has to run on
   the listener's goroutine because a `time.AfterFunc` callback would run beside a notification
   being decided). The loop then skipped the ping whenever a wake ended the wait.
2. The caller's re-arm was bounded to twice the idle period, and the comment claimed the bound
   kept the ping alive.
3. Measured false: the evictor's `wakeIn` returns the **minimum over every deferred room** while
   the ceiling bounds one room's window. Three rooms at twenty-second offsets held every wake
   under the idle period for ever — simulated at **zero pings in 6,000 s**, against 199 for a
   lone room. A stable fixed point: with no ping there is no idle wait to bunch the rooms back
   together.

The fix moved the invariant into the component that owns the connection: a `proven` stamp set by
the LISTEN, by each notification and after each ping, with the ping due on that stamp whatever
ended the wait, and the caller's wake capped at the due time. The two callers that set no hooks
return the idle period unchanged and were exercised rather than assumed inert.

Partial hook combinations, from the same seam: a caller that set `NextWake` without `OnWake`
panicked on its first wake, and a `Listener` with channels and no `OnNotify` panicked on its first
notification. Both now read as absent, because one caller's nil field would otherwise take every
listener in the process with it.

## What the suite does not cover

The cap is computed from the guard's own stamp, so no build has one without the other and the
committed row cannot discriminate them. The **ping decision itself is separable**: reverting it
alone leaves every row green. Where a mechanism has no test of its own, its comment is the
guarantee — which is why the comments that taught the retired model had to be swept
(docs/solutions/best-practices/a-retired-model-is-swept-by-belief-then-by-symbol-INFRA-66.md).

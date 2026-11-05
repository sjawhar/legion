---
title: "Work detached from its caller needs its own mark, deadline, freshness rule and waiter"
category: architecture-patterns
tags:
  - singleflight
  - context
  - shutdown
  - go
  - connection-pool
date: 2026-11-02
status: active
module: packages/envoy/internal/dispatch/architecture
related_issues:
  - "sjawhar/legion#2641"
---

# Work detached from its caller needs its own mark, deadline, freshness rule and waiter

Extends docs/solutions/architecture-patterns/a-waiter-that-abandons-a-shared-read-leaves-its-reservation-with-the-read-pr2323.md,
which covers the reservation an abandoning caller must leave behind. These are the four other
things a shared, detached flight owes, found while replacing a per-project mutex held across
GitHub calls with `singleflight` keyed on the source.

## Rules

- **`context.WithoutCancel` detaches cancellation, not values.** Every value the starter carried
  rides along, marks that name a resource the caller owns included. A flight that inherits an HTTP
  request's pool mark makes its own next acquire a nested one, and a tracking wrapper that is a
  no-op when a mark is present hands the shared mark back instead of taking a fresh one. Take the
  flight's own mark immediately after detaching, before anything opens a transaction. Low
  concurrency and `-race` do not show this.
- **The context that bounds the work must not be the context that records the failure.** The flight
  runs under its own timeout, counted from when it began rather than from when any caller arrived;
  the outcome is written on the undeadlined parent. Pass both (`syncCtx, base`) and write the record
  on `base`, or the timeout destroys the only evidence that it happened — the row the ticker and the
  settings page read. A cancelled caller no longer suppresses that record either, which is a
  behaviour change worth naming where callers can see it.
- **A shared answer has no single caller, so freshness is judged at the moment the source was
  read, not at the moment the flight started.** Keep the time the flight asked the remote for its
  head (and, for one that failed earlier, the time it began), and refuse to answer a caller that
  arrived after that moment: it must start a flight of its own. Without the rule, a write followed
  by a refresh joins a flight that already read the pre-write head and reads its own write as absent.
- **Detaching creates a shutdown obligation: name the waiter it joins and the resource it must
  beat.** The importer's `Close` cancels the flights and waits for them, and it is called inside the
  background wait, which shutdown runs **before** `waitUntilIdle` and `pool.Close` — otherwise a
  sync still fetching when the process stops writes into a closing pool. Make it safe to call twice
  and on a nil receiver, and say in its doc which waits do *not* cover it.

## Evidence

- `packages/envoy/internal/dispatch/architecture/flight.go`: `WithoutCancel` and the mark it must not
  inherit (:106-118), the flight's own deadline (:124-127), `servedAt` (:88-95) and its refusal at the
  join, the `errSuperseded` retry chain kept inside one flight (:128-134).
- The record on the parent: `Importer.failed` (`importer.go:327`), called with `(ctx, parent)`.
- `Importer.Close` and its doc naming the shutdown order (`importer.go:112-124`), wired at
  `cmd/dispatch/main.go:707-709` inside `waitBackground`.
- Measured: a sibling source's sync of one project no longer waits out a slow fetch of another
  (6.031 s blocked at base `0642a37abdaa`, 0.024 s at head), and two concurrent syncs of one source
  cost one installation lookup and one head lookup instead of two (#2641's `E2E (implementer)` line,
  E-1).

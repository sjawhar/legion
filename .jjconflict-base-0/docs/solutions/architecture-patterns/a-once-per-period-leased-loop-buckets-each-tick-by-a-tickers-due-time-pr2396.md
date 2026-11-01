---
title: "A once-per-period leased loop buckets each tick by a ticker's due time"
category: architecture-patterns
tags:
  - leased-loop
  - periodic-loop
  - ticker
  - duty-exclusive
date: 2026-10-28
status: active
module: packages/envoy
related_issues:
  - "sjawhar/legion#2396"
---

# A once-per-period leased loop buckets each tick by a ticker's due time

- A gate that admits one pass per period puts each tick in the period of the moment the tick was
  **due** (`floor(due / period)`). Never read the clock at the gate, after the lease is taken, and
  never use the database's `now()`, even minus a measured age. Any delay before that read moves a tick
  that was due just before a boundary into the next period. The next tick, on time, then finds that
  period served and is refused, and a lone process loses a period.
- A fixed-cadence loop takes its due times from `time.NewTicker`: the value each tick delivers. Go
  delivers the first tick a slow receiver overran at once, with the moment it was due, and drops
  only the ticks after it.
- Do not re-arm a timer after each pass for a fixed cadence. Each tick is then due one interval
  plus the pass's time after the one before, the due times drift across the period boundaries, and
  whole periods get no pass.
- Do not compute the schedule from one clock reading taken at start. That schedule ignores a
  wall-clock step, so two processes label periods differently until one restarts.
- A loop paced by its own passes (it waits after each pass, or steps faster while work remains) is
  a different shape. Give it a helper of its own, named for that shape. Do not fold both shapes into one
  helper whose behaviour depends on the wait the pass returns.
- Prove a lone fixed-cadence loop whose passes take a real share of the interval: each pass must
  record the period after the one its predecessor recorded.

## Evidence

- `packages/envoy/internal/dispatch/duty/exclusive.go:105` (`periodBoundary`) and `:179`
  (`Exclusive` takes `due`). The red tests `exclusive_latency_test.go:39` and `exclusive_test.go`
  (`TestExclusiveCountsALateTickInThePeriodItWasDueIn`) fail when the gate buckets its own moment
  (13 of 13).
- `packages/envoy/internal/dispatch/duty/ticker.go:74-75`: `RunLeased` runs on `time.NewTicker`.
  `:109`: `RunLeasedAfterEachPass` is for the reference-excerpt, block-index and edit-notice loops.
- `duty/run_leased_period_test.go:24` has 400 ms passes on a 1 s interval. It failed against
  `RunLeased` with a timer re-armed after each pass (commit 73dbc2c5): "1 period(s) between them
  got no pass" about every two and a half passes. The schedule computed from a start reading
  (6afef8ce) passed that test but missed a clock step. 0a38d434 replaced it with the ticker.

---
title: "A periodic pass starts from main under RunLeased with a tick at start"
category: architecture-patterns
tags:
  - leased-loop
  - periodic-loop
  - duty
  - restart
date: 2026-10-29
status: active
module: packages/envoy
related_issues:
  - "sjawhar/legion#2444"
---

# A periodic pass starts from main under RunLeased with a tick at start

Extends docs/solutions/architecture-patterns/a-once-per-period-leased-loop-buckets-each-tick-by-a-tickers-due-time-pr2396.md.

- Start a periodic maintenance pass from `cmd/dispatch/main.go` under `duty.RunLeased`. Never
  start it inside a subsystem that a setting can turn off (the outbox runs only with NATS).
- When the pass must run however often the process restarts, give it `duty.TickAtStart`. A
  per-process timer whose first tick is one period after boot never fires in a process that
  restarts more often than that period.
- Add its lease to the leases observed in
  `TestALeasedLoopsPassesNeverOverlapAcrossBothProcesses`, or say there why it cannot be
  observed.

## Evidence

- Before sjawhar/legion#2444, the daily compaction lived in `outbox/publisher.go`. It ran only
  inside `outbox.Run`, which starts only when NATS is set up, and its first tick came 24 h after
  boot.
- After: `cmd/dispatch/main.go` runs `go documentService.RunCompactionSweep(ctx, leases)`, and
  `docs/compaction_sweep.go` calls
  `duty.RunLeased(ctx, leases, compactionSweepLease, compactionSweepPeriod, duty.TickAtStart, ...)`.
  `cmd/dispatch/multiprocess_test.go` observes `document-compaction`: one process ran it and the
  other skipped. In the real-process proof with `DISPATCH_NATS_DISABLED=1`, the restart's sweep
  ran at start.

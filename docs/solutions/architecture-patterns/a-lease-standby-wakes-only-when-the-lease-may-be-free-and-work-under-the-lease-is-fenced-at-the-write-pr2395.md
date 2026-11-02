---
title: "A lease standby wakes only when the lease may be free, and work under the lease is fenced at the write"
category: architecture-patterns
tags:
  - duty-hold
  - lease
  - single-publisher
  - fencing
  - nats
  - standby
date: 2026-10-28
status: active
module: packages/envoy/internal/dispatch
related_issues:
  - "sjawhar/legion#2395"
---

# A lease standby wakes only when the lease may be free, and work under the lease is fenced at the write

Applies to any singleton loop built on `duty.Hold` (`internal/dispatch/duty/hold.go`), the
Dispatch outbox's publisher being the first.

- **Give the standby a wake channel of its own.** Feed it only what can mean the lease is free:
  the release NOTIFY (`duty.HoldReleasedChannel`), a timer to the lapse read from the database
  clock (`duty.Expiry`), a new LISTEN connection (a release sent while it had none is lost), and a
  change of the process's own connection to whatever the work needs. Never let the holder's work
  signals (event inserts, commits) reach it: each one then makes every standby write the lease row.
- **A standby that gave the lease up for losing its dependency proves the dependency answers
  before it takes the lease back.** A NATS connection cut silently still reports itself
  connected, so a check of `Connected()` alone lets the process take the lease straight back and
  lose it again, round after round.
- **Fence at the write, not at the start of the batch.** Check the lease's deadline as the last
  step before each send, after any wait for a connection (`bus.Client.WindowUntil` answers
  `bus.ErrWindowDeadline`); guard every database write that only the holder may make on the
  lease row in the same statement (`exists (select 1 from duty_holds where name = … and holder =
  … and expires_at > now())`); and on every path that finds the lease gone, release it so its
  renewal stops.
- **Test the silent cut with a frozen connection, not a closed one.** `testnats.Proxy.Freeze()`
  keeps the TCP connection open and moves no bytes; `Cut()` only tests the loud case.

## Evidence

- `packages/envoy/internal/dispatch/outbox/lease.go:74` (`standbyWake`), `:236` (`standby`) and
  `:274` (`answering`, with `probeBeforeHold` at `:82`). With one shared wake channel, a 200-comment
  two-process run made 308 inserts into `duty_holds`; with the standby's own channel it made 3.
  The tester's `TestAStandbyDoesNotTryTheHoldForEveryEventTheHolderWrites`
  (`outbox/lease_test.go`) failed with 98 takes for 100 events before the fix.
- Without the probe, the silent-cut row of the two-process suite took over in 16 s or more in
  three of four runs; with it, about 7 s.
- `internal/bus/async.go:121` (`WindowUntil`), `:200` (`expired`, checked first in every send) and
  `:211` (`pinToSend`, checked again after the pin). Before the post-pin check, a send that waited
  for a connection answered `nil` 1.08 s after a 200 ms deadline and was stored
  (`TestAWindowSendsNothingPastItsDeadline`).
- `internal/dispatch/outbox/publisher.go:554-555`: the record's back-off and release writes join
  the holder check in the same statement, so a process frozen past its deadline (SIGSTOP) cannot
  overwrite its successor's schedule (`TestARecordFromAProcessThatLostTheHoldWritesNoBackOff`).

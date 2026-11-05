---
title: "Pin the zero of a time-since signal for the process that never saw the event"
category: testing
tags:
  - mutation-testing
  - clocks
  - health-check
  - regression-lock
date: 2026-10-30
status: active
module: packages/envoy
related_issues:
  - "sjawhar/legion#2504"
---

# Pin the zero of a time-since signal for the process that never saw the event

Extends docs/solutions/testing/mutation-proof-probe-tests.md.

That document owns the method: mutate the production path and see whether a test notices. This is
the mutation a signal of the form "how long since X" always needs.

- **Test the process that never saw X.** Advance a whole bound before any X happens and assert the
  signal reads zero, with no error and the healthy answer. Every test that drives X first stays
  green under a change that starts the clock at construction — and that change degrades the whole
  fleet one bound after start-up, with nothing wrong anywhere.
- **Name the mutation the lock is for, and record its failure text**, not the word "fails", so a
  later reader can re-run it.
- **Expose the clock the production path reads** so the bound passes without waiting it out, and
  drive it from a holder the test owns, guarded for the goroutines the production code reads it
  from. A test that sleeps a real bound either does not run or hides a race.
- **One clock knob, every time-based part of the same subsystem.** When a constructor option
  already exists for one timer, widen its documentation and reuse it rather than adding a second.

## Evidence

sjawhar/legion#2504 added `HookStall`, how long a bus client's reconnect hooks have been due
without a run that succeeded, and `/healthz` answers a non-healthy status word once it reaches
`bus.RepairGrace`. Every test of it drove a reconnect first, so a change marking the hooks due at
the client's first connection would have left every one of them green while every listener in the
fleet answered that status word a minute after it started, having lost nothing.

The lock is the first block of `TestHealthzAnswersAHookStallPastTheRepairGrace`
(`packages/envoy/cmd/listener/healthz_hook_stall_test.go`): before any hook is added and before
any reconnect, `clock.Advance(bus.RepairGrace)`, then `HookStall()` must be `(0, nil)` and
`/healthz` must still answer 200 `healthy`. The mutation it is for is one line in the client's
construction, `c.hooksDueSince = c.now()`; with it the test fails

```
HookStall a whole bus.RepairGrace after the first connection = 1m0s <nil>, want 0 and no error
```

and without it the suite passes. The clock is the existing `bus.WithReconnectErrClock`, whose
documentation was widened from "the throttle of the warning a failed reconnect attempt logs" to
every time-based part of the bus's reconnect reporting, and the holder is a mutex-guarded
`hookStallClock` in the test file; `packages/envoy/internal/bus/restart_test.go` drives the same
knob to see the bound pass.

The request came from the issue's architect at review round 1, after the reviewer observed that
nothing in the tree pinned the zero.

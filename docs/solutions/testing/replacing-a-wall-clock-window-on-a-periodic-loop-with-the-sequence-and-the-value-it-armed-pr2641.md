---
title: "Replacing a wall-clock window on a periodic loop with the sequence and the value it armed"
category: testing
tags:
  - go
  - timers
  - recorded-sequence
  - flaky-tests
  - jitter
date: 2026-11-03
status: active
module: packages/envoy/internal/dispatch/redeliver
related_issues:
  - "sjawhar/legion#2641"
  - "sjawhar/legion#2665"
---

# Replacing a wall-clock window on a periodic loop with the sequence and the value it armed

Extends docs/solutions/testing/prove-call-order-with-a-recorded-sequence-not-microtask-counting.md,
which records a sequence to prove call order. This is the same instrument applied to a *duration*:
how a loop that sweeps, then waits, then sweeps again is held to its period without a window on
the clock that a loaded machine can leave.

## The shape

- **Record the loop's acts in order, from the one goroutine that makes them**, each with the time
  it happened: the work's own observable (a GitHub listing), the end of a pass (its summary log
  line), and **the wait the loop armed, as its value** (`wait 1s`). One goroutine makes all three,
  so the recorded order is the order they happened in.
- **Assert the expected sequence first**, built from the row's fixed draw: `list`, `end`,
  `wait <d>`, `list`, `end`, … A loop that never draws loses every `wait` entry; one that draws
  once reuses a value; one that passes a period over as served shows **two `wait`s in a row**; one
  that stopped sweeping shows no `list` at all. Four defects, one comparison, no clock.
- **Keep one wall-clock assertion, and only the lower one:** each listing comes no sooner than the
  wait armed before it. A loaded machine only lengthens that gap, so the assertion cannot flake.
- **The upper bound does not disappear — it moves off the clock.** "At most 1.1 intervals" is now
  checked exactly, as the armed value in the sequence, which is stricter than the window it
  replaced.

## The two traps

- **The hook's unit is not the caller's.** The production loop arms `jittered(interval)`
  (`sweep.go:277`) while the test hook is `jittered = func(d) { return d + draw(d/10) }`
  (`export_test.go:9-15`), so the recorder inside the draw sees a tenth of the interval and must
  reconstruct what was armed: `events.add(fmt.Sprint("wait ", 10*n+drawn))`
  (`sweep_lease_test.go:235`). A recorder placed mid-expression records the hook's unit and the
  sequence then pins a number nothing arms.
- **The recorder's anchor is a log string.** The end of a pass is recognised by
  `msg="webhook redelivery sweep"` in the logger's bytes, so renaming that line silently empties
  half the sequence. Where you copy this pattern, copy the obligation with it: the anchor belongs
  in a named constant or an assertion that the anchor matched at least once.

## What the clock is still for

A hang guard, and nothing else: a bound far from the work's own duration (`hangGuard = sweeps * 10
* interval`, `sweep_lease_test.go:217`, against about 3.3 s of real waiting), printed in the
failure (`…, within the %v it was given`, `:266`), and inside the step's own `go test -timeout`
so a hang prints the dump that names it. **Once the failures are reported by the sequence, only a
run that records nothing at all reaches that guard**, so a change of reporting re-derives the
controls: the mutations that fail the sequence never exercise the bound.

## Evidence

- The shape is sjawhar/legion#2665's (`sweepEvents`, `recordingGitHub`,
  `packages/envoy/internal/dispatch/redeliver/sweep_lease_test.go:151-197`), adopted by
  sjawhar/legion#2641's forward merge; the guard and its printed bound are #2641's.
- Controls run against it, each a scratch mutation of `Run` restored byte for byte afterwards
  (`sha256 17c6416f…` before and after each): `time.NewTimer(interval + interval/10)` fails the
  sequence (`did ["list" "end" "list" "end" …], want ["list" "end" "wait 1s" …] first`); a period
  that never advances fails it as two waits in a row; `duty.Exclusive` skipped fails it at 22.9 s;
  and a loop blocked before its first sweep fails the guard itself at 41.7 s with `did []`.
- Under load at that head: 50 runs, one process each, under two CPU burners on a two-core pod,
  50 passed, 0 failed.

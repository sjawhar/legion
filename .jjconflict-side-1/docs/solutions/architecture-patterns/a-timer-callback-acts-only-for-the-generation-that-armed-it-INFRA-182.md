---
title: "A timer callback acts only for the generation that armed it"
category: architecture-patterns
tags:
  - go
  - timers
  - afterfunc
  - concurrency
  - test-fakes
date: 2026-10-25
status: active
module: packages/envoy
related_issues:
  - "INFRA-182"
  - "sjawhar/legion#2170"
---

# A timer callback acts only for the generation that armed it

- `time.Timer.Stop` does not wait for a callback that has already started. A callback whose timer
  was stopped can still run after the state it was armed for has been replaced.
- When a `time.AfterFunc` callback acts on shared state, capture in the closure the generation (or
  a token for each arm) it was armed for. Act only when that value is still current, and check it
  under the same mutex as the code that clears the timer and moves the generation.
- A path that is not a timer, and reports for whatever is outstanding (a failed repair, say), reads
  the current generation and goes through the same check.
- A test fake for `AfterFunc` keeps every callback it was handed, in order, so a test can fire a
  stale callback after the next arm. A fake that keeps only the last callback cannot express the
  race.

## Evidence

INFRA-182 (sjawhar/legion#2170), round 1 of review. A `kvwatch` watcher that stops on its own arms a
one-minute wait, which logs `watcher was not repaired` unless a repair clears it first. Without a
token, the following sequence was possible:

1. Wait A fires, and its goroutine blocks on `mu`.
2. A successful `Rewatch` clears A.
3. The new watcher ends and arms wait B.
4. A's callback takes `mu`, consumes B, and reports `no repair within 1m0s` for a stop that is only
   moments old.
5. B's own minute then passes silently.

The fix (`packages/envoy/internal/kvwatch/kvwatch.go`, `reportUnrepaired(reason, generation)`) keeps
the generation `consume` runs for. Only `watch` and `Stop` move the generation, and each clears the
arm under the same lock. `repair_test.go`'s fake `armed.fires` keeps every callback, and
`TestAWaitARepairClearedReportsNotTheNextStop` fires them in that order. Run against a local
nats-server, the test failed before the fix with exactly that ERROR, and passed after.

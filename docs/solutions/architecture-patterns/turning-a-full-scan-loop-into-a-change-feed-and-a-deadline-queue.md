---
title: "Turning a full-scan loop into a change feed and a deadline queue"
category: architecture-patterns
tags:
  - go
  - event-driven
  - change-feed
  - deadline-queue
  - timers
  - mutation-testing
  - benchmarks
date: 2026-10-27
status: active
module: packages/envoy
related_issues:
  - "sjawhar/legion#2367"
---

# Turning a full-scan loop into a change feed and a deadline queue

Use this when a loop wakes on a timer and walks a whole cache to find the few entries that need
work, and you replace it with "work only on what changed or fell due".

## Rules

**Design**

- **Mark changes inside the cache's own write functions, and only for a real change:** a higher
  revision, a key not cached before, or the removal of a cached key. Never mark an equal revision,
  since a watcher replay (a reconnect, a rewatch) delivers every key again at the revision you
  already hold. A missed mark leaves an entry unprocessed for good, because no full scan remains.
  A false mark costs only one look, so the consumer re-reads the cache and decides again before it
  acts.
- **Replace the per-tick checks with one pure `schedule(state, now)`.** It returns what to do now
  and the first millisecond at which that answer changes. Share any check that both the queue and
  the action path use as one function, so the two cannot disagree. An entry no time can change
  (settled, not ready) gets no queue entry, and only a change brings it back.
- **Keep passes on the old timer's grid when callers can see the timing.** A change wakes the
  loop at the next grid instant, never at once. After a pass, the timer is armed for the first
  grid instant at or after the earliest deadline, and strictly after the pass just run. Queue
  every attempted key again at `now`, whatever the outcome. The action's own writes feed the
  change feed, so a loop that ran at once on each change would retry a failing action as fast as
  the store can write.
- **A reset of the source resets every structure the consumer derived from it:** the queue and
  any "held" set. Each of them needs its own test (below).

**Tests**

- **Test `schedule` against the old checks at every millisecond around each boundary.** Assert
  that each returned deadline is the first millisecond at which the verdict changes, and that
  each table row reaches every verdict it expects, so no row passes without testing anything.
- **When a branch's only effect is internal state, assert on the function's own output.** Break
  that branch alone and watch the test fail. A leftover queue entry for a key the cache no longer
  holds changes no publish and no count, so only the pass's "next deadline" output shows it. Pick
  inputs that make the leftover visible without a sleep, for example a debounce so long that the
  entry cannot come due during the test.
- **Pair every "nothing happened" assertion with a count proving the entry was examined.** A
  consumer that never saw the entry also does nothing. Give each simulated replica its own
  consumer, because one shared queue makes the second replica find nothing to do.
- **To prove "no pass ran", poison an output every pass writes.** Set it to an impossible value
  and check that the value survives. Before an idle window that follows a replay, wait for a
  barrier: write a marker the cache applies but never queues, and wait until its revision is
  applied. You need no test-only counter in production code.
- **For timing tests, bound both sides and require exactly one event.** The event must not happen
  before its deadline (check the recorded time), must happen by the deadline plus one tick plus
  slack, and a few ticks later must still have happened exactly once. Remove the deadline re-arm
  once and confirm the test fails.
- **Never put a wall-clock guard in a required test** ("fail if setup took longer than 1 s"). On
  a loaded runner it turns the required check red. Choose inputs that need no guard instead.

**Measurement**

- **Commit the benchmark before the production change.** It may call only functions both base
  and head have, so move any production function you remove into a `_test.go` helper with the
  same signature. Make the benchmark prove it stayed on the path it measures: no publish, and a
  revision that rises at each change op.
- **Check that the load harness the spec names actually runs the path.** If it does not, measure
  the real binary: seed the store, read `utime + stime` from `/proc/<pid>/stat` over a window,
  base and head on the same machine and the same seed.

## What a later change must keep

- Every cache write goes through the marking functions.
- `schedule` mirrors every time-based predicate. A new time rule that `schedule` and its
  millisecond test do not both cover never fires.
- One consumer per change feed (a second attach panics), and tests detach in `t.Cleanup`.
- The due batch is collected before any action runs, and no lock is held during the action,
  because the action's writes take the cache's lock.
- `timer.Reset` without draining the channel is safe only under Go 1.23+ timer semantics.

## Evidence

- Change marks: `packages/envoy/internal/cistore/cistore.go`, `cacheStateLocked` and
  `evictCachedLocked`. The rewatch half of `TestTheSummaryLoopSleepsWhileNothingChanges` fails
  when the mark condition is `>=`.
- `schedule` and the shared `claimFresh`: `packages/envoy/internal/cistore/loop.go`.
  `TestScheduleMatchesThePredicates` scans every millisecond around D, G and the claim's stale
  point.
- Tick grid and spin bound: `StartSummaryLoop` (`max(passed+1, gridIndexAtOrAfter(...))`).
  `TestAFailingPublisherIsRetriedOncePerTick` fails at 193 attempts per second when a wake runs a
  pass at once.
- Invisible branch: the recreated-bucket test first passed with the queue reset removed, and
  waited on a 1 s wall-clock guard (review round 2). Commit b1ce6a9d asserts the pass's empty
  queue, and the test now fails with either reset removed.
- Examined-count pairing: commit 46034eb2 added held-count assertions in `legacy_test.go` and
  moved `TestSummaryTickConcurrentExactlyOnce` to two stores.
- Measurement: `summary_bench_test.go` was committed at 3819fca8, before any production change.
  The load reproduction the spec named mounts no GitHub route, so the real listener with 30,000
  seeded records was measured instead: 0.4438 cores idle at base, 0.0003 at head.

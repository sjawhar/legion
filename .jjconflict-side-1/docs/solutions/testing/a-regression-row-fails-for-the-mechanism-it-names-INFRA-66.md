---
title: "A regression row fails for the mechanism it names, or it is not a lock"
category: testing
tags:
  - regression-tests
  - negative-control
  - attribution
  - multiprocess
date: 2026-10-23
status: active
module: packages/envoy/cmd/dispatch
related_issues:
  - "INFRA-66"
  - "sjawhar/legion#2087"
---

# A regression row fails for the mechanism it names, or it is not a lock

Extends docs/solutions/testing/a-mutation-that-stays-green-because-the-platform-implements-the-behaviour-too-INFRA-43.md.
That document covers a row kept green by the **platform** implementing the behaviour too, and its
remedy is to take the environment's implementation away. The delta here is the in-tree cousin —
another recovery path **in the same system** — where you cannot remove the competitor, and the
remedy is a budget below it plus an asserted attribution.

With the mechanism removed, require two things: the row fails, **and** the failure is
attributable to that mechanism.

- **Bound the budget between the mechanism's measured latency and the next-fastest rescue's.** A
  generous deadline lets another path satisfy the row.
- **Assert who produced the effect and how many times**, not only that the effect happened.
- **Assert the precondition the construction rests on**, and *fail* the run when it is not met,
  so a slow machine says so instead of silently measuring another scenario.
- **A control that reaches the right answer through a second mechanism proves nothing about the
  first.** Remove every cooperating edit, not the one you happen to have in your hand.
- **When a row locks a pair and reverting one half leaves it green, write that limit into the
  row's own documentation and name the row that would discriminate.** Otherwise a later reader
  deletes that half, finds the suite green and concludes it was dead code.
- A negative result keeps its numbers and is recorded as a result; a row that cannot be made to
  discriminate is dropped rather than landed as a row that proves nothing.

## Evidence

Two committed two-process rows were meant to lock a deferred re-examination that closes a stale
document room.

- One passed **with the mechanism unwired**: its thirty-second budget admitted the durable gate's
  own settlement rescue, measured at 6.018 s against the window's 1.95 s. Its own comment named
  that rescue; nothing bounded it away.
- The other never checked its precondition — the trailing write landing inside the two-second
  eviction interval that makes its notification one the interval refuses. Measured gaps were
  91–164 ms, so it passed; a slower machine would have measured an immediate eviction and still
  passed.

Both were rewritten to fail the run when the gap reaches the interval, to credit the close to the
evictor's own second eviction rather than to a socket ending, and — for the settlement row — to a
four-second budget between the two measured numbers. Each was then driven with the mechanism
unwired through a build overlay, and with the budget deliberately widened back to thirty seconds
the row fails on the **attribution** instead, which is what shows the two assertions independent.

The control lesson arrived separately and cost a round: a fix made of two cooperating edits (a
stamp and a cap derived from it) had a control that reverted only one of them, and the row stayed
green, because the other half still produced the right outcome. Only reverting both shows the
mechanism. The pair's non-discrimination is now written into the row's own documentation, with
the discriminating row named — a callback that itself overruns the time the cap would have left.

Related: docs/solutions/testing/race-regression-tests-that-fail-before-the-fix.md (prove the
pre-fix failure mechanically; a bounded wait needs a hard assertion that the awaited thing
happened).

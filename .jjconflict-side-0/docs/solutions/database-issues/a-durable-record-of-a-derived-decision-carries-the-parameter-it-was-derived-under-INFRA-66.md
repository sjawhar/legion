---
title: "A durable record of a derived decision carries the parameter it was derived under"
category: database-issues
tags:
  - periodic-work
  - multiprocess
  - durable-state
  - clock-skew
date: 2026-10-23
status: active
module: packages/envoy/internal/dispatch/duty
related_issues:
  - "INFRA-66"
  - "sjawhar/legion#2087"
---

# A durable record of a derived decision carries the parameter it was derived under

- When a gate stores a **derived** value — a bucket, an aligned boundary, a rounded figure, a
  hash — store the parameter that produced it beside it, and let a stored value count only
  against a comparison made with the same parameter.
- Compute the derivation with the one clock every comparer shares. For work spread over
  processes that is the database's clock, never a process's.
- Cross the parameter as an exact integer. An equality decided by a float's rounding is the same
  defect in a new coat.
- Err toward one extra pass. The other direction is a loop that stops, and because the record is
  durable the stop survives every restart.
- Drive a parameter change in **both** directions. One direction green is the normal outcome of
  the wrong design.

## Evidence

A periodic maintenance loop had to run once per period across two processes. Three designs, each
right against the case it was built for:

1. **A window measured back from now** (`began_at > now() - period`). For a loop whose period is
   its own ticker's interval this reduces to comparing this tick's lease-acquire delay against
   the previous tick's — a coin toss. Measured at **one** process, which is what the deployment
   runs between rolls: ran 7 skipped 3, 6/4, 6/4 over ten one-second ticks, and the same
   signature in a browser suite's single-process log, where a skipped tick had fired 18 ms before
   the one preceding it.
2. **A bare bucket number**, `floor(epoch / period)`. One loop alternates its own period — two
   seconds while drift remains, five minutes when none does. Its first drift step recorded
   896333757 on the two-second scale; every later interval tick compared that against 5975558 on
   the five-minute scale, found it larger, and skipped. For ever, durably, so a restart skipped
   too.
3. **An aligned boundary without the period**, proposed as the fix for (2). Red five runs out of
   five in the growing direction and green three out of three in the shrinking one: a two-second
   boundary is within two seconds of now, while the same tick's five-minute boundary is on
   average 150 seconds behind it.

What shipped stores the aligned boundary **and** the period that produced it, as the exact
nanoseconds the caller passed, and counts a stored boundary only against a tick of the same
period. A period change then costs one extra pass and the loop returns to its own scale.

The migration's comment carries the sentence a future reader needs, because the column looks
redundant: the period is not redundant with the boundary and must not be dropped as if it were —
a boundary carries no record of the scale that produced it.

A residual this shipped with, worth pricing before it bites: the row is keyed by the lease alone,
so during a rolling update that **changes** a loop's period the two builds hold different
parameters for one lease, neither counts the other's record, and the gate is off for that loop
for the length of the roll. Keying the row by lease and parameter closes it.

Related: docs/solutions/architecture-patterns/a-lease-is-a-name-and-the-name-is-its-blast-radius-INFRA-66.md
(why the lease and the record are two mechanisms, not one).

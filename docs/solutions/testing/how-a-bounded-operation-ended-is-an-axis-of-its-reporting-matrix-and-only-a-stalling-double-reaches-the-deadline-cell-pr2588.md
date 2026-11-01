---
title: "How a bounded operation ended is an axis of its reporting matrix, and only a stalling double reaches the deadline cell"
category: testing
tags:
  - context
  - table-tests
  - test-doubles
  - logging
date: 2026-11-01
status: active
module: packages/envoy/cmd/listener
---

# How a bounded operation ended is an axis of its reporting matrix, and only a stalling double reaches the deadline cell

Extends docs/solutions/architecture-patterns/a-stall-that-returns-from-a-record-two-paths-share-is-fixed-where-the-record-is-read-pr2446.md, which states the matrix discipline:
give every cell one reporter and prove each with a test, before review finds the missing cells for you.
This is the axis that discipline is missing.

- For a bounded background operation, what is reported and at what level is a function of three things, not two:
  which step failed, what the authority says about the work, and **how the context ended** - still alive, the operation's own deadline, or the parent cancelling.

- Enumerate that third axis as rows of one table test before you touch any single branch.
  A guard written for one ending silences the others:
  quieting "the context ended" to keep a shutdown out of the error log also quiets the operation's own expiry, which is the failure the error exists for.

- A fault double that fails immediately can never reach the deadline cell.
  A timing-shaped policy needs a double that *stalls* past the window as well as one that fails at once.

## Evidence

The kept role lane's handover in sjawhar/legion#2588 took four review rounds over the same forty lines, each fix exposing the next cell:

- A failed lane-head read ended a claim's handover at Debug, reporting nothing.

- The same fault on the other read, the holder resolution, found one round later.

- The retry that fixed them had no exit for a role nobody holds, so an undecodable cursor was retried for the whole ten-minute window.

- The shutdown guard written for that round, `ctx.Err() != nil`, then silenced the window's own deadline:
  ten minutes of a claimed role's failing lane ended with nothing above Debug.
  The reviewer named why every test had missed it: "every failing read in them fails at once, so the window always ends during a pause."

The settled code is one `retry()` with the rule stated once, `errors.Is(ctx.Err(),
context.Canceled)` rather than `ctx.Err() != nil`, and one exception written where it is made:
a walk cancelled at its confirm write left a copy forwarded and unconfirmed, so that ending reports even under a shutdown.
The test is `TestAHandoverReportsAHolderReadThatFailedAndNotOneItsShutdownCancelled` with rows `failed`, `cancelled` and `stalled`, the third driven by a session bucket that blocks past a short window.
Only the `stalled` row fails at the commit before the fix.

For the Go idiom itself, see docs/solutions/database-issues/record-a-timeout-only-when-your-own-deadline-fired-use-withtimeoutcause-pr2397.md, which prescribes `context.WithTimeoutCause` and `context.Cause`:
that page existed four days before this pull request and was not read, which is half of why this took four rounds.

## Applies to

Every shutdown-quieting guard, every "log only real failures" filter, and every retry loop whose report depends on why it stopped.

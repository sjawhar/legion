---
title: "A test of a read that defers under shared state retries only what the route's own bound admits"
category: testing
tags:
  - postgres
  - events-horizon
  - flaky-tests
  - test-helpers
date: 2026-11-03
status: active
module: packages/envoy/internal/dispatch
related_issues:
  - "sjawhar/legion#2688"
---

# A test of a read that defers under shared state retries only what the route's own bound admits

Extends docs/solutions/testing/code-that-waits-on-instance-wide-database-state-is-tested-under-a-loop-that-holds-that-state-pr2392.md.

That note says to run the package under a loop that holds the instance-wide state and fix every
test that then fails. It does not say what the fix may be, and the obvious fix — wrap the read in
a retry loop — absorbs the defect the test exists to catch. These rules shape the wait instead.

- **Time every call of a read whose route publishes a bound past which it answers partially
  rather than failing.** An answer returned inside that bound was not held back, so it is
  everything the read owed: a predicate that fails on it fails the test now. Only an answer that
  spent the bound is read again, under a wait of the test's own. Never wrap such a read in a
  blanket retry loop: one turns a missing, duplicated or wrongly stamped row into a timeout at the
  outer wait, where it reads as somebody else's open transaction.
- **Such a loop needs no sleep between reads.** Either the read came back inside the bound, which
  is a failure, or it already spent the bound. A sleep there only lengthens the window the shared
  state is held.
- **The rule holds only for a write that committed before the read.** A row an asynchronous
  worker writes after the test's own write (a reconciliation tick, a background index) has no
  bound to spend, so the first read answers fast and short and the test fails at once, loudly and
  wrongly. Await such a row on its writer, not on the route's bound.
- **An empty answer proves nothing until a read you proved was not held back.** A "want none"
  assertion placed before any such read passes whenever the shared state is held, which is the
  very condition the test is written for, so a regression that widens the filter stays green under
  a hold. Put the zero-edge read after an awaited one and say in a comment why it is decisive.
- **A test of an id-cursor read takes its next cursor from the id under test, not from a second
  read of the log's head.** "Nothing new since the newest id" is satisfied by any head at or above
  the row's own event, so it proves nothing about which event stamped the row. Read the id of the
  write under test, assert the row carries it, and take the next read from that same id, as the
  caller the route documents would. A test may take its *first* cursor from the log's head only
  when the write under test draws its id after that read; say so at the call site, because the
  route's published rule forbids a live caller to read `max(events.id)` at all.

## Evidence

In sjawhar/legion#2688, `TestAnAnsweredAsksPartEdgesAreListedSince` took one
`GET /api/v1/references?since=` read as final. That route lists only edges at or below the event
log's safe horizon and waits at most `referencesSinceWait`, one second, for it
(`packages/envoy/internal/dispatch/api/references.go`, `referencesSinceWait` and `sinceHorizon`,
which falls back to the horizon it has rather than failing). The horizon is instance-wide, and the
holder inside that very package is the test process's own `storetest` database clone and drop, so
the test failed about one run in six on main: measured in an issue pod, 25 of 40 runs passed with
a transaction held 2 s of every 2.5 s and 0 of 10 with one held open throughout, while 40 of 40
passed with nothing holding one.

`awaitSinceRead` (`packages/envoy/internal/dispatch/api/references_test.go`) is the two-armed wait:
it fatals at once on an answer returned inside `referencesSinceWait` ("the horizon held nothing
back, so that is every edge it owes") and re-reads only an answer that spent it, bounded by
`eventFeedWait`. Under the same 2 s of 2.5 s hold the five tests that read `?since=` then passed
200 of 200, with 211 of their 813 references reads held past one second; with each of the three
converted call sites given a count the data cannot produce, each went red on a read that answered
in 13-40 ms, in 0.50 s, 1.14 s and 2.53 s, none of them retried.

The helper this change deleted, `awaitGraphEdges`, was the blanket loop: it re-read any wrong
answer for up to `eventFeedWait`, with a 10 ms sleep between reads, so a missing or extra edge on
an unheld read was masked. A hold that never ends is not a flake to retry out either: under one
open transaction that never commits, no bounded re-read can succeed, and the test fails after
`eventFeedWait` naming the hold (`docs/testing.md`, "A test that reads
`GET /api/v1/references?since=` reads again").

The cursor rules are that test's own shape: `answeredEventID` (`ask_parts_test.go`) reads the
`ask.answered` event the answer's part rows are stamped with, the awaited read asserts
`*edge.SourceSeq` is that id, and the second read is taken at that same id — the poller's own
next read — where it had been a second read of the issue's newest event, which any later id
satisfies. Its first cursor is the log's head, which the call site defends: the answer draws its
event id after that read.

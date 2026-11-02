---
title: "A divergence test drives the source through the path only the source takes"
category: testing
tags:
  - negative-control
  - ygo
  - replay
  - test-input
date: 2026-11-02
status: active
module: packages/envoy/internal/dispatch/docs
related_issues:
  - "sjawhar/legion#2485"
---

# A divergence test drives the source through the path only the source takes

- When a copy can diverge from its source only for operations the source performs one way and the copy another, the test's input must include the operations only the source performs that way. Input that reaches both sides the same way makes both behave alike, and the test passes with and without the fix.
- Before you write, move or port such a test, list which kind of operation each step produces, and interleave the kind that only the source makes.
- Then run its negative control: remove the fix and see the test fail. A divergence test that has never failed has not shown it can.

## Evidence

- ygo runs a format cleanup after a remote apply and not after a document's own transaction; every copy of a room is built with that cleanup off (docs/solutions/envoy/every-copy-of-a-ygo-room-is-built-by-one-constructor-with-the-remote-only-format-cleanup-off-pr2496.md).
- The tester's cold-read churn test on sjawhar/legion#2485 fed every edit to the room as a browser update or a published write, both remote applies, which the room cleans up after too. Moved into `format_replay_test.go`, it passed with `WithFormatCleanup(false)` removed, at 3 seeds by 40 rounds and at 8 seeds by 150.
- 073590ff added the room's own writes (`writeInRoom(..., serviceWrite, ...)`, `format_replay_test.go:232`, `:311`, `:337`), with the room's writes last so no later browser update sweeps the gap; without the option all 3 seeds fail.

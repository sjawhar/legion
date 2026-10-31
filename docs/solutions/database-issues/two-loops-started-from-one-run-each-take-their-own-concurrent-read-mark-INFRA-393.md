---
title: "Two loops started from one Run each take their own concurrent-read mark, and a test drives both through Run"
category: database-issues
tags:
  - dispatch
  - store
  - connection-pool
  - goroutines
  - embedqueue
date: 2026-10-26
status: active
module: packages/envoy/internal/dispatch/store
related_issues:
  - "INFRA-393"
  - "sjawhar/legion#2208"
---

# Two loops started from one Run each take their own concurrent-read mark, and a test drives both through Run

- A context marked with `store.WithTransactionTracking` stands for **one** caller. The Dispatch
  pool refuses that caller a second connection while it holds one (`store/pool.go`), and two
  goroutines that share the mark count as one caller holding one connection.
- So when a `Run` starts two long-lived loops (a periodic pass beside a scan or poll loop), give
  each loop its own mark in `Run`: `store.ForConcurrentRead(ctx)` once per loop
  (`embedqueue/queue.go`, `Run`). `packages/envoy/AGENTS.md`'s `ForConcurrentRead` paragraph
  says this for reads side by side within one request. It holds just as much for background loops,
  where the failure shows only under load.
- Test the pair through the real entry point, `Run`, with one loop holding a cursor open while
  the other runs a statement. Tests that call each loop's function on its own never see the two
  share a mark.

## Evidence

INFRA-393 added a coverage pass (`embedqueue.Cover`, at boot and every 10 minutes) beside the
queue's scan loop, both started by `embedqueue.Run` on one tracked context. Every unit test
called `Cover` or the scan's batch function directly and passed. The tester booted the real
binary with 50 pending 32,000-character comments. The scan loop held a cursor open while it
embedded them, and the pool refused the coverage pass's first statement:
`dispatch: second pooled connection requested while one is held`, then
`coverage pass failed; the next one runs on schedule`. The items written before the triggers
then waited ten minutes for the next pass.

The tester's red test `TestRunsCoveragePassIsNotRefusedWhileTheScanLoopHoldsACursor`
(`embedqueue/coverage_test.go`) runs both loops through `Run`. It failed on the pre-fix tree
with the same log line and passes with one `ForConcurrentRead` per loop. Live, the same 50 dense
comments and 5 comments no trigger saw all went current in 68 seconds, with no ERROR.

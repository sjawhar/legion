---
title: "Compare a start-time statement count only where no statement is in flight"
category: testing
tags:
  - race
  - flaky-test
  - pgx-tracer
  - storetest
date: 2026-10-30
status: active
module: packages/envoy
related_issues:
  - "sjawhar/legion#2391"
---

# Compare a start-time statement count only where no statement is in flight

- Before you compare a statement count with another counter, read when the recorder counts.
  `storetest.StatementRecorder` records in `TraceQueryStart`; its `TraceQueryEnd` does nothing.
  So a statement is counted while it is still in its round trip.
- Compare the two counters at a point where no statement can be in flight. For example, take the
  count inside the pass itself, after the statement returned and while the pass still holds its
  lease. Or stop every runner (cancel, then wait for it to return) before you read either count.
- Never poll both counters from outside while the loops run. A poll that falls inside a round trip
  sees one counter ahead of the other, and the test fails by chance, more often under `-race`.
- Prove the fix with `go test -race -count=200` on that one test, and with a negative control that
  breaks the pairing on purpose and fails at once.

## Evidence

Pull request sjawhar/legion#2391. `TestTheMaintenanceLoopSweepsOncePerPeriodAcrossTwoRunners`
compared the key-sweep deletes (`StatementRecorder`, counted at query start,
`packages/envoy/internal/dispatch/store/storetest/traced.go:83-90`) with a resume counter that
rose only after the sweep returned, while `waitUntil` polled every 10 ms. The reviewer found the
race. Commit 84d139a3081a moved the comparison into the pass's resume
(`packages/envoy/internal/dispatch/api/maintenance_test.go`). 200 of 200 runs passed under
`-race` in 450.8 s, and a temporary second key sweep per pass failed at once with "pass 1 resumed
deliveries after 2 key sweeps".

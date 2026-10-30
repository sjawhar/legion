---
title: "Work a request starts past its client runs on Dispatch's backgroundWork, never a WaitGroup of its own"
category: envoy
tags:
  - shutdown
  - goroutines
  - waitgroup
  - dispatch
  - forward-merge
date: 2026-10-29
status: active
module: packages/envoy/internal/dispatch/api
related_issues:
  - "sjawhar/legion#2397"
---

# Work a request starts past its client runs on Dispatch's backgroundWork, never a WaitGroup of its own

- In the Dispatch server, start any goroutine a request leaves running (a retry, a delivery, a
  record written after the answer) with `s.detached.goDetached(func() { … })`. Do not add a
  `sync.WaitGroup` field for it, and do not add its wait to `waitBackground`.
- Reason: shutdown calls `waitBackground` once its HTTP drain bound passes, with requests still in
  flight, so work can start after the wait began. `sync.WaitGroup` forbids an `Add` from zero
  while `Wait` runs (it can panic, or the wait returns first and the pool closes under the work).
  `backgroundWork` counts work that starts mid-wait and returns only when it sees none running.
- Keep a separate cap (an `atomic.Int32`) when the work must be bounded; `backgroundWork` only
  counts.
- At a forward merge that brings a shared mechanism like this from main, move your branch's own
  version onto it and delete yours in the merge round, before a reviewer finds the duplicate.
- Test it by starting the work while `waitBackground` already waits (hold the wait open with
  `s.detached.start()`), then check the wait does not return while the work runs. Use a
  registered test server (`newRegisteredTestServer`); `directServer` has no wired background.

## Evidence

sjawhar/legion#2397 retried a failed related-check record on `relatedRecordRetries sync.WaitGroup`.
The forward merge of main 166bc0c4 brought #2449's `backgroundWork`
(`packages/envoy/internal/dispatch/api/server.go:429-470`), and review round 12 asked for the
change. The fix, ca85bfa9, runs the retry at `suggestions.go:863` and adds
`TestRelatedCheckRecordRetryStartedWhileShutdownWaitsIsWaitedFor`
(`related_check_test.go:501`). With the retry on a plain, uncounted goroutine, that test failed:
the wait returned while the retry still held its row.

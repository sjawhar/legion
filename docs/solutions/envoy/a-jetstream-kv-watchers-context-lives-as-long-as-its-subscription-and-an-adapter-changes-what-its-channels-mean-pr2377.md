---
title: "A jetstream KV watcher's context lives as long as its subscription, and an adapter changes what its channels mean"
category: envoy
tags:
  - nats-go
  - jetstream
  - kv-watch
  - goroutine-leak
  - adapters
date: 2026-10-29
status: active
module: packages/envoy
related_issues:
  - "sjawhar/legion#2377"
---

# A jetstream KV watcher's context lives as long as its subscription, and an adapter changes what its channels mean

When you watch a KV bucket through nats.go's `jetstream` package (for example to resume with
`jetstream.ResumeFromRevision`, which the legacy `nats.KeyValue` API does not offer):

- Pass `WatchAll` a cancellable context with **no deadline**. nats.go keeps that context for the
  subscription's whole life and unsubscribes when it is done, so a `context.WithTimeout` ends the
  live watch at its deadline instead of bounding the consumer's creation.
- Cancel that context yourself once the watcher's updates channel closes. nats.go parks a goroutine
  on it and cancels it only inside `Unsubscribe`, which a closed connection refuses. Without your
  cancel, each watcher whose connection closed leaves one goroutine behind for the life of the
  process.
- Do not expect your JetStream context's `nats.MaxWait` to reach the consumer create. The create
  runs on the legacy JetStream context's default wait (5 s in nats.go v1.50.0). If a full
  `WatchAll` on a 10 s context is the alternative path, a slow server fails only the resume, so make
  a resume that cannot start fall back to the full read with one WARN rather than fail the repair.
  Bound only the `js.KeyValue` lookup before the create, which does take a deadline.
- When an adapter puts a second implementation behind an interface an existing reader already
  consumes (here, a `jetstream.KeyWatcher` behind `nats.KeyWatcher`), list every channel and
  sentinel the reader interprets and decide what each means for the new implementation. A probe
  that reads a buffered error destructively (kvwatch's `idleTimeout`) turns a wrong interpretation
  into lost information: gate it on the implementation that produced the watcher.
- Prove the goroutine bound with a test that opens many watchers, closes their connection, waits
  for every updates channel to close, and polls `runtime.NumGoroutine()` back to a settled
  baseline. Run it once with the cancel removed and see it fail.

## Evidence

- `packages/envoy/internal/bus/kvresume.go`: `WatchAllFrom` creates the watch context with
  `context.WithCancel` and no deadline, and `resumedWatcher.forward` cancels it once the inner
  updates close. Its comments cite nats.go v1.50.0: the context is kept and the subscription
  unsubscribed when it is done (`js.go:2053-2057`), the create waits the legacy context's default
  (`jsm.go:1791-1796`, `js.go:299`), and `Unsubscribe` refuses a closed connection
  (`nats.go:5063-5065`).
- `packages/envoy/internal/bus/kvresume_test.go`,
  `TestResumedWatchesLeaveNoGoroutineBehindWhenTheirConnectionCloses`: 40 resumed watchers, the
  connection closed. Without the cancel the count stayed at 57 against a baseline of 25; with it,
  it returned to the baseline.
- `packages/envoy/internal/kvwatch/kvwatch.go`, `watch`: a resume whose watcher cannot be created
  logs `<cache> watcher could not resume; the bucket is read in full` and retries the same watch
  with a full `WatchAll`. Covered by `TestARewatchWhoseResumeCannotBeCreatedReadsTheBucketInFull`
  in `internal/kvwatch/resume_test.go`.
- `packages/envoy/internal/kvwatch/kvwatch.go`, `consume`: the resumed watcher has no idle timer,
  and its `Error` channel carries the watch's end cause just before its updates close. Reading it
  as an idle timeout labelled a healthy resume `timed out`, left its generation incomplete and took
  the cause away from the end. The fix is `timedOut := !resumed && idleTimeout(watcher) != nil`,
  covered by `TestAResumedWatchThatEndsAtItsMarkerIsNotTimedOut`. Both defects were found in review
  of sjawhar/legion#2377.

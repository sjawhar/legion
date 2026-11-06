---
title: "A test that binds and releases a JetStream push consumer waits for PushBound both ways"
category: testing
tags:
  - nats
  - jetstream
  - flaky-test
  - race
date: 2026-10-31
status: active
module: packages/envoy
related_issues:
  - "sjawhar/legion#2391"
---

# A test that binds and releases a JetStream push consumer waits for PushBound both ways

- nats-server applies a push subscriber's interest, and its loss, on the consumer's own goroutine, after the client's subscribe or unsubscribe has already returned. A test cannot tell from the client side whether the server has applied either.
- When a test binds a push consumer and then releases it, wait until `ConsumerInfo(...).PushBound` reads true before the release, and false after it. A wait for false alone can pass before the server applied the bind at all.
- Publish a probe only after both waits. Otherwise the server can apply the bind late and send the probe to the released handle's deliver inbox. Nobody acknowledges it there, so it waits out the consumer's `AckWait` (30 s here), and a fresh subscription is handed nothing within the test's bound.
- A failure with this shape is easy to read: the test's steps take about a second, and then it waits out its whole timeout.

## Evidence

Pull request sjawhar/legion#2391, commit 382271f8. `TestReleaseFailedUnsubscribesWhatARestoreBound` (`packages/envoy/internal/bus/release_test.go`) failed once in about 13 CI runs: "timed out waiting for: the fresh subscription to be handed the message", 11.07 s in all. It did not fail in 60 local runs under `GOMAXPROCS=1` with CPU loaders, so the cause came from reading the code and the CI log. The fix adds `awaitPushBound` and waits for true before each release of a restored binding, in that test and in two drain tests. Under `-race -count=200` the named tests passed 1,400 times, and under `GOMAXPROCS=1` with two loaders 350 times.

---
title: "A request reads the connection's failure record on the goroutine that answers, and flushes beside its wait, not ahead of it"
category: envoy
tags:
  - nats
  - request-reply
  - flush
  - permission-violation
  - bus
date: 2026-10-31
status: active
module: packages/envoy/internal/bus
related_issues:
  - "sjawhar/legion#2395"
---

# A request reads the connection's failure record on the goroutine that answers, and flushes beside its wait, not ahead of it

Extends docs/solutions/envoy/attribute-an-out-of-band-failure-by-its-own-key-and-time-not-the-connections-last-error-pr2508.md.

- **Every path that can answer the send reads the record.** A request-reply call reads the
  per-subject denial record under its own send time (`sentAt`, taken right before the publish)
  as a publish does.
- **Read it only on the goroutine that answers the caller.** A helper goroutine (a flush) can
  still be running after an early answer, and if it reads the record it takes the denial that
  the next send of the subject is owed.
- **Read it once more before answering a timeout.** The error callback that records the denial
  can run after the flush returned.
- **Run the confirming flush beside the wait for the answer, never ahead of it.** nats.go clears
  pending flushes only when the connection goes from connected to reconnecting
  (`clearPendingFlushCalls`). A flush started after that moment waits for its whole timeout, so a
  request whose connection is already gone would answer only at its deadline instead of at once.

## Evidence

- `internal/bus/async.go:358-360` (`sentAt`), `:366-382` (the flush on its own goroutine, with
  the `clearPendingFlushCalls` reason), `:383-391` (`denied`, read on the answering goroutine
  alone) and `:496-510` (the read again before a timeout answer).
- Commit `14170450` moved the flush beside the wait:
  `TestARequestWhoseConnectionIsLostAnswersUnavailable` (`internal/bus/window_request_test.go:195`)
  gave no answer within 10 s, 3 runs of 3, and was the `envoy-go` CI failure at `bcf50ed5`.
- Commit `4fc3e0d2` keyed the request's denial on its own subject and send time:
  `TestEveryDeniedRequestOfOneWindowNamesItsOwnSubject`
  (`internal/bus/request_denied_window_test.go:35`) named another subject's violation for 6 or 7
  of 8 concurrent requests at `14170450`.
- `docs/solutions/architecture-patterns/core-nats-receipt-backed-current-holder-delivery.md`
  describes a flush between publish and receipt wait; that order is the one that hangs when the
  connection is lost after the publish.

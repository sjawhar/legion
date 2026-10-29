---
title: "A new bus error gets its own case at every HTTP edge and request outcome"
category: envoy
tags:
  - error-classification
  - http-status
  - request-record
  - listener
date: 2026-10-28
status: active
module: packages/envoy/cmd/listener
---

# A new bus error gets its own case at every HTTP edge and request outcome

Extends docs/solutions/envoy/classify-a-store-error-by-the-stores-named-answers-and-let-every-other-error-be-the-store-not-answering-INFRA-141.md.

- When the bus layer gains a new named error, find every place that turns a bus error into an
  HTTP answer or a recorded outcome, and give the new error its own case in the same change. A
  default branch that means "the bus did not answer" otherwise takes it, and its body and outcome
  then send an operator after an outage that did not happen.
- When two error classes share one HTTP status, decide the recorded outcome from the error
  (`errors.Is`), not from the status the writer returned.
- Keep the status the caller's retry rule needs: an error the same request can get past later
  (a full stream) stays a 5xx with `Retry-After`; one it can never get past (a permission denial)
  is a 4xx.
- A test of the new case asserts the outcome it must record, not only that the wrong outcome is
  absent, so a later case that records a third outcome still fails the test.

## Evidence

`bus.ErrStreamFull` was added for a full `ENVOY_NOTIFICATIONS` (`DiscardNew`). The listener's
`writeBusUnavailable` answered it under its default branch, `503 envoy: the message bus did not
answer: …`, and `refuseBusUnavailable` recorded `bus_unavailable` because it classified by status
alone. A real listener on a full v2.15.1 stream showed both on `/v1/messages/send` and
`/v1/messages/publish`. The fix answers it under `streamFullPrefix` with its own 503 and
`Retry-After: 1` and records `stream_full`, decided by `errors.Is(err, bus.ErrStreamFull)`
(`packages/envoy/cmd/listener/api.go`, `request_record.go`; sjawhar/legion#2362, commit
859993f7f6cd; red test 3ceb0bead0af).

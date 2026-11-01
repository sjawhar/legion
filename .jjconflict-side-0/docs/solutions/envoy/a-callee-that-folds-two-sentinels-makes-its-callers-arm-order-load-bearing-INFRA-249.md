---
title: "A callee that folds two sentinels makes its caller's arm order load-bearing"
category: envoy
tags:
  - error-handling
  - errors-is
  - nats
  - listener-routes
date: 2026-10-25
status: active
module: envoy
applies_when:
  - You are adding a caller of session.SessionRegistry.Refresh or roleHolderSession
  - You are reordering error arms, or turning them into a switch, in cmd/listener/api.go
  - A caller must tell "the key is absent" from "the key is invalid"
related_issues:
  - "INFRA-249"
  - "sjawhar/legion#2191"
---

# A callee that folds two sentinels makes its caller's arm order load-bearing

## Rules

- **Before you classify an error, read what the callee wraps.** `session.SessionRegistry.Refresh`
  returns `fmt.Errorf("%w: %w", nats.ErrKeyNotFound, bus.ErrRefused)` for a key NATS will not
  take, so **both** `errors.Is` tests are true and only the order of your arms decides the answer.
  Test the narrower sentinel — `bus.ErrRefused` — first.
- **Arm order follows the callee, not a convention of the file.** In the same file,
  `adminInterestsHandler` tests `nats.ErrKeyNotFound` first and is correct, because
  `store.Registry.Get` does not fold. A refactor that makes two such handlers look alike breaks
  one of them.
- **Pin the order with a behavioural test at the route, not with a comment.** A comment is the
  weakest guard there is: the next person reordering two arms for readability, or rewriting them
  as a `switch`, flips a caller's bad input into a retryable failure and nothing goes red.
- **Remember that the far side may not be able to see the difference.** Dispatch's client maps
  `400`, `413` and `404` alike to `ErrNotFound`, so a swapped arm is invisible across the service
  boundary. The listener's own table test is the only thing that holds it.

## Evidence

`sessionEntryHandler` (`packages/envoy/cmd/listener/api.go`) answers
`GET /v1/registry/<session_id>` and must tell three outcomes apart: an id NATS refuses as a KV
key (`400`/`413`, the caller's to fix, no retry can help), a key the bucket does not hold
(`404 {"error":"no live session <id>"}`), and a read that did not answer (`503` with
`Retry-After: 1`). Because `Refresh` folds the first into the second, writing the
`nats.ErrKeyNotFound` arm first would answer `404` to a refused key — a `no live session` a caller
would act on, for an id under which no session can ever have registered.

The guard is two rows added to `TestV1RoutesAnswerAKeyNATSWouldRefuseWith4xx`
(`cmd/listener/api_test.go`): `/v1/registry/<a 4 KiB id>` must answer `413` and
`/v1/registry/ses:bad` must answer `400`. Swap the arms and both answer `404` and the test fails.
The order was found while writing the handler, not by a failure, and the architect's standing
instruction on this branch was that a comment at the line is not enough — pin it with something
that runs.

Why not make `Refresh` stop folding, which would remove the hazard at its source: the role
callers of `roleHolderSession` depend on the fold, since a holder under a refused key is one they
must treat as lapsed. The fold is deliberate; the arm order plus its pin is what that costs.

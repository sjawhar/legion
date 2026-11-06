---
title: "Classify a store error by the store's named answers, and let every other error be the store not answering"
category: envoy
tags:
  - error-classification
  - nats
  - errors-join
  - retry-after
date: 2026-10-27
status: active
module: packages/envoy
related_issues:
  - "INFRA-141"
  - "sjawhar/legion#2167"
---

# Classify a store error by the store's named answers, and let every other error be the store not answering

Extends docs/solutions/architecture-patterns/derive-a-refusals-condition-from-what-the-caller-depends-on-not-from-the-mechanism-that-broke-it-INFRA-249.md.

That note says what a refusal must depend on. This one says which side of the test the default
goes on.

- **Name the store's answers, not the ways it can fail to answer.** A store answers with a short,
  closed set: no key, a key it refuses, a record this build cannot decode, a conditional write
  another writer beat. The ways a call can fail to answer are an open set that grows with each
  client release. Test `errors.Is` against the closed set, and treat every other error as "did not
  answer" (in the listener: `503` with `Retry-After: 1`).
- **Never write a list of "did not answer" errors.** Such a list classifies the first error nobody
  named as an answer, and the caller then acts on a failure as if the store had spoken.
- **An error after the caller's context ended did not answer**, whatever it names: check
  `ctx.Err()` before you classify.
- **A joined error is an answer only when every part is.** `errors.Is` on an `errors.Join` result
  matches any part, so one answered part hides a part that did not answer. Split only a real join
  (its type is the type `errors.Join` returns); a `fmt.Errorf` with several `%w` also unwraps to a
  list, but its parts are one failure and its detail.

## Evidence

- 38feffb0 added `interestReadDidNotAnswer`, a list of eight "did not answer" errors
  (`context.DeadlineExceeded`, `nats.ErrTimeout`, `nats.ErrConnectionClosed`,
  `nats.ErrNoResponders` and four more). nats.go v1.50.0 returns the socket's own `*net.OpError`
  when a request fills its write buffer on a broken connection, which no entry matched, so
  `GET /v1/sessions` listed a live session with no topics. f804579e replaced the list with
  `interestReadAnswered` (named answers only); `TestAnInterestReadFailingWithARawSocketErrorIsTheBusNotAnswering`
  (`cmd/listener/interest_read_socket_error_test.go`) is the guard.
- 74318aa8 applied the same rule to the write routes: `storeAnswered` checks `ctx.Err()` first, and
  `storeAnswer` splits a join by `joinedType`, because `store.Registry.SetRoleWithPrevious` joins a
  lost claim with its holder read and its rollback. Guard:
  `TestAJoinedStoreErrorIsTheStoresAnswerOnlyWhenEveryPartIs` (`cmd/listener/write_routes_frozen_bus_test.go`).
- `bus.Bounded` returns `(value, answered, err)`, so a caller that bounds a call learns whether it
  answered from the bound, not by reading the error.

---
title: "Measure the mechanism, not the symptom: count the work the system does not do"
category: testing
tags:
  - proof
  - negative-control
  - rate-limiting
  - amplification
date: 2026-10-25
status: active
module: packages/envoy/internal/broker
applies_when:
  - The fix changes how much work a request costs rather than what it answers
  - A status code, a message or a count is the same before and after the fix
  - You are about to assert on a response to prove a guard
related_issues:
  - "INFRA-228"
  - "sjawhar/legion#2187"
---

# Measure the mechanism, not the symptom: count the work the system does not do

When the defect is work the system should not have done, the response is the same before and
after the fix, so a test that asserts on the response proves nothing. Count the work.

- **Name the mechanism first, then the observable that moves with it.** "Forged tokens are
  refused" is the symptom and was already true; "a forged token no longer costs an upstream key
  fetch" is the mechanism, and its observable is the fetch count.
- **Count side effects at their source.** A counting round-tripper in front of the issuer, a
  counter on the verifier, a statement count on the store: the collaborator the system reaches
  through is where the number is honest. A log line or a duration is a proxy and drifts.
- **Run the probe against the pre-fix code as a negative control.** A `git archive` of the
  previous head, or the one function restored, driven by the same probe: a probe that does not
  fail there is measuring something else. State both numbers.
- **Where the risk is a race or a concurrency guard, inject the thing the guard stops.** A canary
  that fails the lane, a mutation that defeats the lock, a second process holding the row — a
  clean run of a detector nobody has seen fail is not evidence the detector works.
- **The negative control belongs in the committed test where it can be**, as a second case that
  must behave differently: one address flooded and another answered, a lapsed credential and a
  live one. Where it cannot (it needs the old code), run it by hand and quote both numbers in the
  evidence.

## Evidence

INFRA-228 (sjawhar/legion#2187) closed an amplification: ten routes verified the caller's
Kubernetes ServiceAccount token before any rate limit, and go-oidc's `RemoteKeySet` refetches the
issuer's key set whenever a token names a key id it does not hold. The issuer and audience a
forged token must carry are configuration, not secrets, so an unauthenticated caller bought one
upstream fetch per request.

The status was `401` before the fix and `401` or `429` after, so no status assertion could see
it. The reviewer's deep review reproduced it by counting: 12 forged requests, 12 fetches. The
committed test (`packages/envoy/internal/broker/api/account_limit_test.go`) counts the verifier's
calls under a flood and finds the burst's worth and no more, with a valid token from a second
address answered throughout as the in-test control. The tester's own run quoted both sides: 400
forged requests cost 250 fetches at the fixed head and 400 at the head before it.

Three more proofs in the same change have this shape. A two-process rollout test
(`cmd/broker/multiprocess_test.go`) was run against an export of the pre-fix code and failed in
exactly one subtest, which is what showed it tested the defect rather than the harness. The
machine-exchange revoke race is proven by a test that holds the credential row and checks the
concurrent revoke *waits on that row*, not merely that the final state is right. And the
no-thrash property is asserted on the processes' own logs — the absence of a takeover line —
beside a case where the same line is written, so the absence means something.

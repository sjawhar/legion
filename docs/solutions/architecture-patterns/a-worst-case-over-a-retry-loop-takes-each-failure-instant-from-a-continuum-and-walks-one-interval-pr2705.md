---
title: "A worst case over a retry loop takes each failure instant from a continuum, and walks one interval"
category: architecture-patterns
tags:
  - worst-case-model
  - retry-loop
  - configuration-bound
date: 2026-11-04
status: active
module: packages/envoy/internal/broker/leasetime
related_issues:
  - "sjawhar/legion#2705"
---

# A worst case over a retry loop takes each failure instant from a continuum, and walks one interval

When you bound what a retry loop can survive — a lease, a stop grace, a deadline, a budget — and
the loop computes its next wait **from the moment a failure comes back**, the instant each attempt
fails is a free variable with a whole interval of values, not two.

- Take the failure delay as continuous in `[0, the call's own timeout]`. A call ends at once when
  the connection is refused, at the full timeout when something black-holes it, and anywhere
  between when a proxy gives up on it or a terminating pod resets it.
- Do not model "fails at once or holds to the timeout" and take the worse of the two. It is not a
  bound: an attempt that fails early retries sooner, which can place a **later** attempt just
  inside the outage, where that one then costs the whole timeout. The two-endpoint answer is
  shorter than the true worst case, so a bound derived from it accepts settings that lapse.
- Search the continuum in one forward pass rather than by enumeration. Check that each
  next-launch function is non-decreasing in the instant the previous attempt failed; then the
  instants each step can reach form one interval, and carrying its two ends forward — `lo`, where
  every attempt so far failed at once, and `hi`, where each was held as long as it could be — is
  the whole search.
- Read the answer at **every** step, not only the last, when any wait grows with the failure count
  (a doubling backoff does). A later step can be worse than an earlier one.
- State in the model which side each term is adversarial on, and whether the loop moves its own
  deadline when an attempt fails. Both renewers here keep the expiry of their last answer, so the
  time left always counts down from the attempt the walk starts at; a loop that re-reads a
  deadline on failure needs a different walk.

## Evidence

`sjawhar/legion#2705` (dispatch://INFRA-454) took three review rounds on one model, and the first
two were both endpoint models.

Round 1 modelled each refused renewal as failing at once or at the whole 30 s client timeout, and
assumed the renewal whose timeout crossed the outage's end was the only one worth holding. Round 2
implemented the reviewer's prescription — "take the worse of refused-now and held-to-timeout" —
and was still unsound. Both accepted leases that lapse: `agent-secrets renew` at a 10 s sweep with
a 100 s lease renews at 103.33 s, and `agent-secrets-helper` at a 60 s sweep with a 355 s lease
renews at 358.33 s, each after its lease ran out.

Round 3 (`c18538f7`) made the failure instant continuous. `leasetime.Model.worstRenewal`
(`packages/envoy/internal/broker/leasetime/leasetime.go`) is the interval pass, and its own
comment states the monotonicity the pass rests on and why every step is read: "agent-secrets
renew's backoff grows with the number of failures". `Model.WorstFor`'s doc states the unsoundness
directly: "Taking the whole timeout is not always the worse of those: a renewer that hears sooner
retries sooner, which can put a later renewal just before the broker returns, where that one then
costs the whole timeout."

Both lapses are now pinned rows of `TestSurvivesWalksEachRenewersOwnRetrySchedule`, named "a lease
the two-outcome model accepted". The negative control is one overlay edit that snaps each failure
to the nearer end of its timeout: those rows go red and the model reproduces the old, wrong
acceptance exactly (`33.33+0s, 35.33+0s, 39.33+0s, 47.33+30s, renewed 87.33 s`).

The cost of the endpoint model was the two rounds: the floors it printed were short by up to 9 s
at a 60 s sweep, which is the difference between a configuration the broker accepts and one whose
pods lose their sessions in an outage.

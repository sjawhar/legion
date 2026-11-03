---
title: "When more contenders do not reproduce a race, drive the suspected call directly, and census every site that can produce the symptom"
category: testing
tags:
  - race-conditions
  - reproduction
  - instrumentation
  - flaky-tests
date: 2026-10-30
status: active
module: packages/envoy/internal/bus
related_issues:
  - "sjawhar/legion#2506"
---

# When more contenders do not reproduce a race, drive the suspected call directly, and census every site that can produce the symptom

Extends docs/solutions/testing/widen-the-contender-count-before-calling-a-race-unreproducible.md,
which prescribes more contenders under a recorded load recipe. That is one exit from "it will not
fail here"; this is the other, and it is the one to take when widening has already failed.

- **Drop below the end-to-end test to the one call you suspect, and race that.** The smallest
  direct driver — N clients, one per cluster member, making the single request in a tight loop with
  no setup between rounds — reaches the window orders of magnitude more often than the acceptance
  test that calls it once per iteration. Keep that driver as a committed test when it is cheap.
- **Instrument every site that can produce the symptom's text, not the one the plan names**, and
  log the branch you tolerate as well as the branch that fails. Absence then becomes a result:
  "240 starts never entered the create branch" disproves a named cause, where silence from one
  instrumented site proves nothing.
- **State an absence with its N**, in the pull request and in the handoff, so the next reader can
  weigh it against the one observation that went the other way.
- **A red-on-base from another role or another machine is that role's observation, not yours.**
  Name whose it is. An acceptance criterion whose own command the implementer cannot make fail is
  a plan-time problem: renegotiate it into a command that fails before the plan is approved,
  rather than spending hours of loops proving it rarely.

## Evidence

- The spec behind sjawhar/legion#2506 named a create race (`AddStream` after a clustered `StreamInfo` answered
  `stream not found`). Temporary instrumentation at all four refusal sites — `AddStream`,
  `UpdateStream`, `CreateKeyValue` and the bucket raise — in a scratch copy of `packages/envoy`
  showed no create-branch entry at all in 240 starts, including the tolerated
  `stream name already in use` branch. The create paths (`internal/bus/stream.go:408-424`) were
  left untouched.
- The acceptance command failed 0 of 200 iterations in the implementer's pod (about 35 minutes of
  runtime) and 0 of 30 with its contenders widened from two to eight. Three clients sending one
  stream update at once refused in 6.6 s, 1 of 900 requests, and became
  `TestConcurrentUpdatesOfOneStreamEndWithTheStreamHoldingThem`
  (`internal/bus/stream_update_overlap_test.go:101-148`), which fails 4 of 5 runs at the base.
- The reds that did exist were the planner's 1 of 50, the tester's 1 of 20 and CI run
  40857522211 — three single observations from three environments, each quoted as its owner's.

---
title: "A bound stated as the sum of a try's waits and bounded calls pins its call multiplier with a rig that counts one worst-case try's calls; hand arithmetic undercounts, and short is the one failure the bound must never have"
category: daemon
tags:
  - launch-bound
  - sandbox-runtime
  - timeouts
  - supervise
  - rig-test
  - context-deadline
date: 2026-10-16
status: active
module: packages/daemon/internal/runtime/sandbox
related_issues:
  - "LEGION-694"
  - "sjawhar/legion#1999"
---

# A bound stated as the sum of a try's waits and bounded calls pins its call multiplier with a rig that counts one worst-case try's calls; hand arithmetic undercounts, and short is the one failure the bound must never have

- When a runtime states a bound for one try as the sum of the try's own waits plus `N × apiTimeout`
  for its bounded calls, write the formula as code (`launchBound(bootTimeout, provisionBound,
  terminationGrace)` beside `relaunchCalls`), never a literal duration, and prove `N` two ways: a
  unit test that pins the formula's arithmetic, and a rig test that runs one real worst-case try
  (a replacing launch with every role of the pod) and counts its client requests, mints, store
  reads and launcher frames, asserting `count <= N`. Count `N` generously — every retry branch and
  rare path — because a bound shorter than a legitimate try cuts a live launch off and charges it
  as a failure, while a generous bound only delays the verdict on a try that has already hung past
  every per-step deadline.
- Expect the rig's count to exceed a plan's estimate: the plan guessed one call per role write and
  bind and about 50 minutes at the defaults; the rig measured 46 calls for one replacing try and
  the honest multiplier is 71 (17 per try, 9 per launcher role × 6 roles), 1 h 15 min at the
  defaults. Record the value and its derivation where the operator reads it (`docs/kubernetes.md`)
  and in the pull request, since it is above the number the spec's prose carried.
- Apply the bound inside the runtime as a child context of the caller's (`context.WithTimeout(ctx,
  bound)` from after the pod's launch turn through the launcher's start), keep the failure-path
  cleanups on the parent context with bounds of their own, and tell the two apart for the caller:
  wrap the error only when the bounded context ended while the parent is alive, so a supervisor
  that reads `ctx.Err()` to tell the daemon's stop from a refusal charges the try that hit the bound
  and charges nothing for a try its own context ended.

## Evidence

LEGION-694 (sjawhar/legion#1999) gave `runtime.Runtime` a `LaunchBound()` and the Sandbox runtime
`launchBound = 3·bootTimeout + launcherBackstop(bootTimeout, ProvisionBound) + terminationGrace +
relaunchCalls·apiTimeout` (`packages/daemon/internal/runtime/sandbox/relaunch.go`, applied at the top
of `relaunch` after `lockPod`; `boundRanOut` adds `(the launch try's bound of <d> ran out)` only when
the bounded context ended with the parent alive). The plan's D8 estimated about 50 minutes;
`TestAReplacingLaunchMakesNoMoreCallsThanTheBoundCounts` (`launch_bound_test.go`) counted 46 bounded
calls in one replacing launch on the rig — 37 fake-client requests, 1 provisioning-token mint, 6
credential renders, 1 store read, 1 launcher frame — against `relaunchCalls = 71`, and
`TestLaunchBoundSumsTheTrysWaitsAndCalls` pins `launchBound(120 s, 30 min, 10 s) = 1h15m41s`, above
`bootTimeout + ProvisionBound + terminationGrace` and the spec's 30-minute floor.
`TestALaunchTryThatRunsOutIsChargedAndTheNextTryRuns` drives the real `supervise.Machine` over the
rig with the bound shortened under the boot timeout: the first try ends at the bound, the claim is
charged one launch failure and the next try begins; `TestALaunchTryItsContextEndsIsChargedNothing`
cancels the parent mid-try and sees `supervise: launch cut short; its context ended` with nothing
charged. The reviewer noted that no production caller reads the interface method yet (the runtime
applies its private field), a fast-follow.

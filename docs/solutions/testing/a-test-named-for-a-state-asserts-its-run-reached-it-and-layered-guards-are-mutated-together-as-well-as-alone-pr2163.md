---
title: "A test named for a state asserts its run reached it, and layered guards are mutated together as well as alone"
category: testing
tags:
  - property-test
  - mutation
  - seeds
date: 2026-11-03
status: active
module: packages/daemon
related_issues:
  - "sjawhar/legion#2163"
---

# A test named for a state asserts its run reached it, and layered guards are mutated together as well as alone

Extends docs/solutions/testing/a-load-test-cpu-fix-needs-a-seed-that-reaches-the-changed-code-a-load-that-completes-it-and-the-functions-profile-share-pr2425.md.

- A pinned seed of a nondeterministic model is no evidence that the run reaches the state the test is named for, and a fix can move that state out from under the seed with nothing going red.
  Have the model count the state, and assert the count in the test body.

- Choose seeds that reach it on every schedule you try, and record how often they did.

- When breaking each guard of that state alone fails no test, break them together as well, and report both results.
  Guards that cover for each other are defence in depth; a guard no combination needs is dead code, and only the combined run tells the two apart.

## Evidence

The tester's red test `a9aa0bc0` showed that `TestADecisionHeldInTheOldArchitectsTaskAtAHandOverReachesTheChild`, whose seeds 96, 414 and 432 found its fault, reached 0 hand-overs between a held task and its confirmation at the current code, and passed with every guard of that window broken.
Round 35 (`f2e29c93`) runs seeds 13, 20 and 24 instead.
Each reached the window in three runs of three, and the test now asserts `run.counts.handedBeforeConfirm > 0` (`packages/daemon/internal/daemon/kept_decision_handover_seeds_test.go`).
Breaking any one of four guards alone (the tell's tree check, `ReleaseHeldDecisions`, `rootTask`'s narrowing, the hold's tree check in `record.keptCounts`) failed no test, and no seed among 1-150 at 300 steps.
Breaking three together failed the property test and two held-task hand-over tests.
The reviewer judged the layering defence in depth.

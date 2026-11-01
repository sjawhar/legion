---
title: "A red test's assertion lives where the collaborator that produces the effect is real"
category: testing
tags:
  - red-test
  - test-rig
  - admission
  - import-cycle
  - daemon-tests
date: 2026-10-25
status: active
module: packages/daemon
related_issues:
  - "INFRA-229"
  - "sjawhar/legion#2180"
---

# A red test's assertion lives where the collaborator that produces the effect is real

- Before writing a red test, find the code that writes the effect you assert (grep the store call
  or the outbox kind), and confirm the rig you write in wires that code for real. A stubbed
  collaborator makes the assertion unsatisfiable, so the red can never go green, whatever the fix.
- In `packages/daemon/internal/workflow`, the rig applies facts with `admissionStub{}`
  (`engine_test.go:33`, a no-op), and only `admit.Admission.releaseSlots` frees a slot. `admit`
  imports `workflow`, so no workflow test can use the real admission. Assert a slot in
  `internal/admit`'s tests (which wire both the engine and the admission) or in a live proof, and
  in a workflow test assert workflow's own write (the `linger_close` outbox row a root's close
  queues).
- The implementer handed such a red test does not weaken it quietly: it changes only the
  unsatisfiable line, moves that assertion to a test where the collaborator is real, shows both
  fail without the fix, and records the change in the handoff's `deviations` and the pull request
  body.

## Evidence

- The tester's red test `TestANewPullRequestFromAWaitingRootsBranchLeavesItsMergeStanding`
  (`merge_close_test.go`) asserted the root's phase, its production check and `slots = []`. With
  the fix, the phase and check assertions passed and the slot assertion failed: `slots =
  [LEGION-208], want the closed tree's freed`.
- The slot line became `assertOutboxCount(t, rig.pool, "linger_close", 1)`, and
  `TestAMergedRootWaitsForItsChildrenAndClosesWhenTheLastLeaves` (`admit/finished_root_test.go`)
  got the scenario through the real admission, with `assertSlots(t, pool, nil)` after the close.
  With the fix removed, both tests fail.

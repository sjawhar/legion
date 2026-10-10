---
title: "A proof script that stands in for a human follows the agent's new step: read the state first and expect the agent's outcome"
category: testing
tags:
  - e2e
  - proof-human
  - merge_when_clean
  - prompt-change
  - fake-gh
date: 2026-10-10
status: active
module: scripts/e2e
related_issues:
  - "LEGION-631"
  - "sjawhar/legion#1843"
---
# A proof script that stands in for a human follows the agent's new step: read the state first and expect the agent's outcome

- When a prompt change gives an agent an action a live proof used to perform on its behalf (the
  proof human's merge, a label, a status), the proof's step for that action is part of the change.
  Left as it was, the step meets the agent's result (an already-merged pull request) and the run
  fails on the harness, not on the behaviour.
- Rewrite the step to read the object's state first and branch on it: the agent's outcome (merged,
  armed, enqueued) is the expected path, asserted by its artifact (the merge commit); the hand
  action runs only when the agent did nothing (nothing armed, GitHub reads CLEAN); a hand action the
  service refuses because the agent's landed first is re-read, not failed. Record who acted in a
  variable the stage's note prints (`merge_when_clean_by`, `merge_when_clean_commit`), so the proof
  no longer asserts the actor it used to be.
- Lock the branching with the directory's fake-CLI harness (one case per branch, the bound and the
  poll overridable so a timeout case runs in seconds), and leave the service's own answers — its
  refusal wording, which states merge — to a live run in a scratch repository; a fake that invents
  them pins the author's guess.
- Grep the proofs for every place that performs or expects the old action, not only the helper:
  the stage notes that said `merged by the proof human` were claims the new code made false.

## Evidence

sjawhar/legion#1843: spec v8 made the merger submit its own merge on READY's acceptance, and
`scripts/e2e/stage4b-sandbox-tree.sh`'s `done` and `stage3-devbox-workflow.sh`'s
`ordinary-human-squash-merge` still ran `merge_when_clean` — poll `mergeStateStatus` for CLEAN, then
`gh pr merge --match-head-commit` as the proof human — which fails on a merged pull request. The
implementer named the hazard in its round-8 report; the architect ordered the harness change in the
same round (envoy 7e046fdf). `scripts/e2e/lib/workflow.sh`'s `merge_when_clean` now reads `state`,
`mergeStateStatus`, `headRefOid`, `mergeCommit`, `autoMergeRequest` and `mergeQueueEntry` in one
GraphQL read; `scripts/e2e/lib/merge-when-clean.test.ts` (13 cases, fake `gh`) locks the branches;
the tester's round 7 drove the real answers in `sjawhar/legion-smoke` (#539 read UNSTABLE for its
first seconds and would have been waited on, not merged).

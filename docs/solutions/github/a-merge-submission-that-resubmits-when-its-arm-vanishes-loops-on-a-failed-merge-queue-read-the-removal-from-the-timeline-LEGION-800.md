---
title: "A merge submission that resubmits when its arm vanishes loops on a failed merge queue: read the removal from the timeline, and enqueue only a mergeable pull request"
category: github
tags:
  - merge-queue
  - auto-merge
  - graphql
  - timeline
  - ready_merge
date: 2026-10-17
module: packages/daemon/internal/ghautomerge
related_issues:
  - "LEGION-800"
  - "LEGION-760"
  - "sjawhar/legion#1987"
---
# A merge submission that resubmits when its arm vanishes loops on a failed merge queue: read the removal from the timeline, and enqueue only a mergeable pull request

- **An arm or merge-queue entry that disappears is not a cue to submit again.** A failed `merge_group`
  run takes the pull request out of the queue and leaves its head's own checks green, so nothing
  else moves it. A submission that re-enqueues whenever the entry is gone queues the same failure
  after every run and rebuilds every entry behind it. The daemon's `ready_merge` row now records the
  first arm or entry it sees (`ReadyMerge.Armed`) and, once that is gone, reads why from the pull
  request's timeline (`ghautomerge.removal`).
- **The timeline says why.** `RemovedFromMergeQueueEvent.reason` takes the values `merged`,
  `failed_checks`, `manual` and `merge_conflict` (counted on the deployment repository's last 300
  pull requests, 2026-10-17: 98, 44, 16, 9), and `beforeCommit` is the commit the queue built, whose
  `statusCheckRollup` names each `merge_group` check run with its `checkSuite.workflowRun.url`.
  `AutoMergeDisabledEvent.reasonCode` takes `manually_disabled`, `closed` and
  `merge_commit_blocked`. `RemovedFromMergeQueueEvent` has no `afterCommit` field, and the merge
  queue's branch is gone with its entry (`checkSuite.branch` reads null).
- **A timeline actor is a bot's login without `[bot]`; GraphQL's `viewer` is the same login with it**
  (`sjawhar-agent` beside `sjawhar-agent[bot]`). Compare them with the suffix stripped, or the
  daemon's own disarm reads as a person's removal.
- **On a merge-queue base, `enablePullRequestAutoMerge` enqueues only a pull request GitHub reports
  mergeable now** (`mergeStateStatus` `CLEAN`, `HAS_HOOKS` or `UNSTABLE`); on any other state it
  arms auto-merge. A required check that judges the arm (one triggered on `auto_merge_enabled`) then
  fails. Wait for the mergeable state, then enable; never enable while `BLOCKED` or `UNKNOWN` there.
- **Read the shape of a GitHub answer from GitHub, not from memory**: the removal reasons and field
  names above came from `gh api graphql` against real pull requests, and the decoding was replayed
  on those answers before the tests encoded them.

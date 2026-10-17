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
  (`sjawhar-agent` beside `sjawhar-agent[bot]`). Compare them with the suffix stripped. The actor
  alone still does not make a removal the daemon's: GitHub names the App that enabled auto-merge as
  the actor of its own disables (a `merge_commit_blocked` disable on the deployment repository names
  `sjawhar-agent`, the viewer), so the daemon's own disarm is the viewer *and* the reason a disarm
  leaves (`manually_disabled`, or a `manual` dequeue). Any other removal the timeline gives the
  viewer is GitHub's own, and the architect's notice says so.
- **A person's dequeue cancels the `merge_group` runs in flight, so a cancelled run is no failed
  merge** (a real `manual` dequeue left 53 runs cancelled, 20 skipped and 15 passed). `failed_checks`
  always counts, `manual` and `merge_conflict` never do, and a reason GitHub gives no other meaning
  counts only with a run that failed on the queue's commit by Legion's rule for a check run
  (`classify.RunResult`), cancelled excepted.
- **A merge group's checks run past one page of 100** (a real `failed_checks` removal carried 118,
  with a failure on each page). Read `statusCheckRollup.contexts` page by page through
  `object(oid:)` on the queue's commit, which resolves after its branch is gone, and only for the
  removals whose reason needs them: the queue's `merged` removal, read on every retry while GitHub
  has yet to show the merge, never waits on a page or fails on one.
- **On a merge-queue base, `enablePullRequestAutoMerge` enqueues only a pull request GitHub reports
  mergeable now** (`mergeStateStatus` `CLEAN`, `HAS_HOOKS` or `UNSTABLE`); on any other state it
  arms auto-merge. A required check that judges the arm (one triggered on `auto_merge_enabled`) then
  fails. Wait for the mergeable state, then enable; never enable while `BLOCKED` or `UNKNOWN` there.
- **Read the shape of a GitHub answer from GitHub, not from memory**: the removal reasons and field
  names above came from `gh api graphql` against real pull requests, and the decoding was replayed
  on those answers before the tests encoded them.

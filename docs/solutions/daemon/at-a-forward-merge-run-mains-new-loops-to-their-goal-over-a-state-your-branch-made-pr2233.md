---
title: "At a forward merge, run main's new loops to their goal over a state your branch made"
category: daemon
tags:
  - forward-merge
  - background-work
  - semantic-conflict
date: 2026-10-27
status: active
module: packages/envoy/internal/dispatch
---

# At a forward merge, run main's new loops to their goal over a state your branch made

Extends docs/solutions/daemon/a-state-that-switches-off-a-background-duty-does-its-last-run-at-the-transition-and-a-durable-sweep-covers-what-arrives-after-pr2233.md.

- Keep a list on your branch of each duty a state switches off, and of each write the branch makes
  outside the normal path.
- At every forward merge, search main's delta for code that waits for one of those duties or reads
  what those writes leave.
- For each one, run main's own path to its goal over a fixture your branch produced, in deploy order,
  and assert main's goal. A green merged build and your own tests do not show this: main's code is
  correct on main, and your tests do not know main's new goal.

This is the census docs/solutions/daemon/a-forward-merge-brings-mains-new-outbox-writer-in-unscoped-the-commit-scope-census-runs-again-and-the-racy-wakes-error-line-is-the-tests-witness-LEGION-694.md
describes, applied to main's waiters rather than its writers.

## Evidence

sjawhar/legion#2233's close writes ask states into a closed issue's live document with no version,
and no settlement runs while the issue is closed. Main's #2210, merged in at 4440fc7b, skipped a
document whose live text is ahead of its latest version until a settlement wrote the next version.
On a closed issue that never came. The merge's proof (build, store, duty and the change's own close,
repair and block-index tests) passed, while the block-index loop logged `live_ahead=1
owed_documents=1` forever. The reviewer found it (thread PRRT_kwDORFy7ds6wlYBJ), and seven fixing
commits followed, from 67f7f265 to ec86df97. The test that pins main's goal is
`TestTheBlockIndexAfterTheBootRepairIndexesAClosedIssuesDocument`: migration 0121's owed state, the
boot repair, then the real loop, with nothing left owed and every block reference resolving.

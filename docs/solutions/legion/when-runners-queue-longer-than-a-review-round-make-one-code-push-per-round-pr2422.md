---
title: "When runners queue longer than a review round, make one code push per round"
category: legion
tags:
  - ci
  - concurrency
  - skip-checks
  - review-rounds
date: 2026-10-28
status: active
module: .github/workflows
related_issues:
  - "sjawhar/legion#2422"
---
# When runners queue longer than a review round, make one code push per round

Extends docs/solutions/legion/an-implement-handoff-push-runs-ci-in-full-and-cancels-the-code-heads-runs-cite-the-tips-runs-and-give-the-lane-the-code-heads-image-LEGION-631.md.

- A code push cancels the queued `CI` run of the push before it, as a handoff push without the
  trailer does. When runners queue longer than a review round, every run is cancelled before it
  starts, and no required check ever completes.
- Put each round's fixes and any merge of main into one code push: commit the fix, merge main on
  top of it, and push once. After that push only handoff pushes follow, each with
  `skip-checks: true`.
- Merge main only when GitHub reports the pull request `CONFLICTING`. A merge to stay current is
  one more code push that cancels the run.

## Evidence

- `.github/workflows/ci.yaml` sets `concurrency.group: ci-<pull request number>` with
  `cancel-in-progress` on `pull_request`.
- sjawhar/legion#2422: every `Tests` run (the workflow before `ci.yaml`) on the branch up to 0c77df5c was cancelled before a
  runner took it, because the next code push cancelled it. The
  architect then required one code push per round: 0c77df5c (fix faeb4057 and main 8b4a862b), and
  e6f60222 (fix fc24d837 and main e82b013b), each followed only by skip-checks handoffs.

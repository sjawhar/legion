---
title: "GitHub's reviewDecision stays REVIEW_REQUIRED after a GitHub App's APPROVED or REQUEST_CHANGES: read latestReviews or the daemon's review state, never reviewDecision"
category: github
tags:
  - reviewDecision
  - latestReviews
  - github-app
  - branch-protection
  - mergeStateStatus
  - pr-review
  - envoy-review-payload
date: 2026-09-14
status: active
module: packages/daemon/src/daemon/reducers.ts, packages/daemon/src/daemon/catchup.ts
problem_type: correctness
severity: medium
related_issues:
  - "LEGION-93"
  - "sjawhar/legion#1082"
applies_when:
  - A role or script asks GitHub whether a Legion pull request is approved or has changes requested
  - You are choosing which review signal a gate, a wake, or a READY packet keys on
  - mergeStateStatus is BLOCKED with a green rollup and you wonder what is blocking
---

# `reviewDecision` stays `REVIEW_REQUIRED` after a GitHub App's review

## Observed (PR #1082, 2026-09-14)

After `legion-reviewer[bot]` (the review App) submitted `APPROVED` at `e7e0b7ed`, and earlier after
its `REQUEST_CHANGES` at `ba2adbdb`, `legion gh -- pr view 1082 --json reviewDecision` answered
`REVIEW_REQUIRED` both times, while `--json latestReviews` listed `legion-reviewer APPROVED` and
`mergeStateStatus` stayed `BLOCKED` on a green rollup. `reviewDecision` is GitHub's summary of the
*required-review* rule in branch protection; a review by a GitHub App installation does not count
toward that requirement, so the field never leaves `REVIEW_REQUIRED` however the App votes. It is
not "no review yet"; it is "no review that branch protection counts".

## Rules

- Anything that asks "did the reviewer approve / request changes?" reads a per-review signal:
  `pr view --json latestReviews` (the latest state per author, with `commit_id`),
  `api repos/{owner}/{repo}/pulls/{n}/reviews` (every review, pinned to its `commit_id`), or the
  daemon's own state. Never `reviewDecision`.
- The daemon already does this. `state.prs[...].reviewDecision` is **not** GitHub's field: the
  `review` reducer (`reducers.ts`) derives it from Envoy's normalized `pull_request_review` payload
  — `payload.state` lowercased, head-gated for `approved` against `pr.headSha`, recorded from any
  commit for `changes_requested`, dropped on every new head by `resetPrHead` — and the worker
  catch-up reports it as `review: approved | changes_requested | pending`. `/worker/spawn`'s
  corrective-round status write keys on that daemon field. Keep it so: a future reader tempted to
  "confirm" against GitHub's `reviewDecision` would see `REVIEW_REQUIRED` forever and undo a real
  approval.
- `mergeStateStatus: BLOCKED` with a green check rollup and an App approval on the head is branch
  protection waiting for a *counted* approval the App may never supply. On this deployment the
  merge queue merges under its own authority after `READY`; the merger's gate facts are the
  reviewer's `APPROVED` at the exact head SHA (verified through `latestReviews`/`reviews`), the
  green run, and the thread state — not `reviewDecision`, and not `mergeStateStatus` leaving
  `BLOCKED`.
- The same App boundary explains the thread rule: the review App can reply on a thread but cannot
  resolve it or push, so the implementer's/merger's `legion threads resolve` closes the threads the
  reviewer has `Accepted:` (see `packages/daemon/src/daemon/AGENTS.md`, GitHub Apps).

## Related

- `../legion/one-role-keyed-table-decides-which-github-app-acts.md` — which App each role acts
  as, and why the review App holds no `contents` permission.
- `conflicting-pr-gets-no-pull-request-ci.md` — `mergeable`/`mergeStateStatus` and when a PR gets
  no CI at all.

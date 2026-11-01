---
title: "Give one job its own gate derived from an anchored shared filter, and let the workflow-only pull request prove it"
category: github
tags:
  - ci
  - paths-filter
  - dorny-paths-filter
  - yaml-anchor
  - negative-control
date: 2026-10-29
status: active
module: .github/workflows
---

# Give one job its own gate derived from an anchored shared filter, and let the workflow-only pull request prove it

Extends docs/solutions/github/gate-an-expensive-step-inside-a-required-job-with-a-paths-filter-that-runs-only-on-pull-requests-and-prove-the-skip-with-a-stacked-scratch-pr-INFRA-584.md.

- **Before you change one job's definition, read its `if:` gate.** If the job's paths filter does
  not list the workflow file that holds the job, a pull request that changes only that job skips
  it. Then "the job passes on the pull request" cannot hold, and the change does not run on main
  after the merge either.
- **Do not widen a filter that other jobs share.** Every job gated on it then runs on every edit
  of the workflow. Put an `&anchor` on the shared key and add a key of its own for the one job:
  the workflow file plus `- *anchor`. Point only that job's `if:` at the new key, and add the key
  to the `changes` job's `outputs`. dorny/paths-filter flattens the nested alias. If a comment
  says that anchored keys gate no job, correct it in the same edit.
- **Check the derivation statically.** Load the `filters:` string with a YAML parser, flatten each
  key and assert that the new key is the shared key plus exactly the paths you added.
- **The pull request that adds the gate is its own proof.** It changes only the workflow file, so
  at its head the job must show `success`, not `skipped`. A job still on the shared filter must
  show `skipped` at the same head. The `changes` job's log must print
  `Filter <shared> = false` and `Filter <new> = true`. Use the same job's last run on main before
  the change as the negative control. No scratch pull request is needed for this half.
- **The shared-paths half is not exercised by this pull request.** That half is a file inside the
  shared filter changed with no change to the workflow. Prove it on the pinned action's own code
  (docs/solutions/github/prove-a-path-filter-change-with-the-pinned-actions-own-code-and-a-skip-and-run-pair-of-scratch-pull-requests-based-on-the-branch-INFRA-583.md),
  or name it on the pull request's `Not proven / risk` line.

## Evidence

- `dispatch-browser-multiprocess` was gated on `dispatch` (`envoy-and-contracts.yaml:1090` at
  be4567e3), whose list left out the workflow file. #2476 changed only workflows and docs, and its
  head shows that job `skipped`.
- Commit 08cbb0a2 anchors the key (`dispatch: &dispatch`, line 647). It adds
  `dispatch_browser_multiprocess` (lines 705-707: the workflow file plus `- *dispatch`) and gates
  the job on it (line 1099). It also corrects the anchor comment (lines 182-183). Adding the
  workflow to `dispatch` itself was rejected: that would run the `dispatch` job, 34-45 minutes and
  red on main, on every edit of the workflow (`plan.json` `adjacentGapNamed`).
- Flattened, the filters gave `dispatch` 41 entries and `dispatch_browser_multiprocess` 42, the
  workflow file being the only extra entry.
- At head 08cbb0a2:
  - `dispatch-browser-multiprocess` job 123224515403: `success`.
  - `dispatch` check run 123224516911: `skipped`.
  - `changes` job 123220975388: `Filter dispatch = false`, `Filter dispatch_browser_multiprocess = true`.
  - Negative control: main's job 123138185821 (head af1ff09c).

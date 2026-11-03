---
title: "Count a workflow's runner waits as its longest needs chain of jobs that request a runner, and keep only stages a credential or data boundary forces"
category: github
tags:
  - github-actions
  - runner-queue
  - concurrency-limit
  - release-workflow
  - job-graph
date: 2026-10-28
status: active
module: .github/workflows
related_issues:
  - "INFRA-576"
  - "sjawhar/legion#2358"
---

# Count a workflow's runner waits as its longest needs chain of jobs that request a runner, and keep only stages a credential or data boundary forces

## The rule

1. On an account whose standard runners share one concurrent-job limit (every GitHub user
   account: larger runners are only for organizations), each link of a `needs` chain is a wait in
   the same queue pull-request CI fills. A workflow's waiting cost is the length of its longest
   `needs` chain of jobs that request a runner. Jobs inside a reusable-workflow call count; a job
   its `if:` skips does not.
2. Merge adjacent stages unless one of two things forbids it, and write the reason for each stage
   that stays:
   - a credential boundary: a job holding write scopes or a deploy key cannot be the job that ran
     the dependency code it publishes;
   - a data dependency: a reusable call's `with:` reads only jobs it `needs`, and a job's outputs
     exist only once it ends.
3. Decide each package from its own tags rather than with a changed-paths job, which adds a stage
   and a run-history lookup; a replaced pending run then loses nothing, since every commit since a
   package's last tag is released by the next run.
4. Do not look for a separate pool in runner labels on a user account: there is none.

## Evidence

- `.github/workflows/release.yaml` lines 61-65 (the three stages and their reasons) and 993-995
  (the image build needs `legion_build` for the plugin pair). sjawhar/legion#2358's stage table:
  10 stages before, 3 after; probe run 40451765299 on sjawhar/legion-smoke showed the 3-stage
  chain.
- Before the change, 54 of 65 `release.yaml` runs on main since 2026-10-26 were replaced while
  pending, and merge-to-published medians were 270-374 minutes per package (the PR body's
  measurement, script in https://github.com/sjawhar/legion/pull/2358#issuecomment-6434025628).
- https://docs.github.com/en/actions/reference/limits, "Job concurrency limits".

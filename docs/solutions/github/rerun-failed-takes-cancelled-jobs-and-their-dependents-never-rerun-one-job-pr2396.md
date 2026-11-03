---
title: "rerun --failed takes cancelled jobs and their dependents; never re-run one job"
category: github
tags:
  - gh-run-rerun
  - github-actions
  - ci
date: 2026-10-28
status: active
module: skills/legion-worker
related_issues:
  - "sjawhar/legion#2396"
---

# rerun --failed takes cancelled jobs and their dependents; never re-run one job

Extends docs/solutions/github/pull-request-trigger-paths-follow-the-pr-head.md.

- To clear a failed or cancelled job, run `gh run rerun <run> --failed`. Despite its name, it
  re-runs the failed and the cancelled jobs, and every job that depends on them. It runs nothing that
  succeeded.
- Do not re-run one job with `--job <id>`. The final job of a routing workflow then reads its
  dependencies' cancelled results again and stays red.
- Do not re-run the whole run. It runs every green job again.

## Evidence

- `skills/legion-worker/references/merge-gate.md:56-62`.
- Scratch pull request #2408, run 40521376914: attempt 2 (`--failed`) re-queued lint and test and
  copied typecheck as it was (same `started_at`/`completed_at`). Attempt 3, the whole-run control,
  re-queued all three.

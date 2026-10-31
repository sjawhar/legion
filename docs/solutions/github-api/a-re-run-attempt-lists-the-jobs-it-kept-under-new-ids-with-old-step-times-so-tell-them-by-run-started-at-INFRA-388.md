---
title: "A re-run attempt lists the jobs it kept under new ids with old step times, so tell them apart by run_started_at"
category: github-api
tags:
  - github-actions
  - workflow-run
  - run-attempt
  - re-run
  - production-check
date: 2026-10-25
status: active
module: packages/daemon
related_issues:
  - "INFRA-388"
  - "sjawhar/legion#2203"
---

# A re-run attempt lists the jobs it kept under new ids with old step times, so tell them apart by run_started_at

- `GET /repos/{o}/{r}/actions/runs/{run}/attempts/{n}/jobs` lists every job of attempt `n`. That
  includes the jobs a "re-run failed jobs" kept from the earlier attempt. Each kept job has a
  **new job id**, **`run_attempt: n`**, and the **earlier attempt's `started_at`/`completed_at`**
  on the job and on each of its steps.
- To ask "did this step run in this attempt", compare the step's `started_at` (or its
  `completed_at` when it has no start) with the attempt's own `run_started_at`, from
  `GET …/attempts/{n}`. A step that started before the attempt started was kept. Never filter on
  the job's `run_attempt` or on the job id being new: GitHub sets both on kept jobs.
- A dedupe key built from the job id does not catch a kept job, because the id is new. Without
  the time test, one deploy wakes once per attempt.
- Before a plan's decision rests on how GitHub shapes an Actions answer, check it against a real
  re-run. Find one with
  `gh api 'repos/<o>/<r>/actions/workflows/<file>/runs?per_page=100' --jq '.workflow_runs[]|select(.run_attempt>1)|.id'`,
  then read both attempts' jobs. Fakes shaped from the plan's own belief pass every unit test.

## Evidence

- sjawhar/legion run 39304834688. Attempt 2's `run_started_at` is 2026-10-19T19:00:02Z. Its job
  `changes` is id 118247428626, with `run_attempt: 2` and started 18:42:59Z, the same times as
  attempt 1's job 118239084369. Only `worker_image / build` really ran again (19:00:08Z).
- INFRA-388's plan held that `run_attempt` equality drops an earlier attempt's jobs. The
  implementer's throwaway live driver read the real attempt and showed the filter dropped nothing.
  The fix is commit 1e00c2e1: `checkDeployRun` in `packages/daemon/internal/daemon/outbox.go`
  logs `succeeded only in jobs this attempt kept from an earlier one`. The locks are the
  kept, re-run and mixed cases in `outbox_deploy_test.go`.
- The live proof published attempts 1 and 2 of run 39304834688 for a check on #2018. Attempt 1
  opened look 2; attempt 2 opened nothing.

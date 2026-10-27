---
title: "A head is green by the newest run of each check name, workflow and event"
category: github
tags:
  - ci
  - checks
  - required-checks
  - gh-api
date: 2026-10-26
status: active
module: packages/dispatch
related_issues:
  - "INFRA-256"
  - "sjawhar/legion#2247"
---

# A head is green by the newest run of each check name, workflow and event

Extends docs/solutions/github/a-green-lane-name-is-not-a-run-read-the-run-and-page-the-check-list-INFRA-31.md.

That document says to judge the required set by check name. Name alone is not GitHub's key:

- To tell whether a pull request head is green from the REST API, read every page of
  `commits/<sha>/check-runs` and of `actions/runs?head_sha=<sha>`, and join them on
  `check_runs[].check_suite.id` = `workflow_runs[].check_suite_id`. A check run carries no event;
  every workflow run, a re-run attempt excepted, has a check suite of its own.
- Keep only the run with the highest id for each check name + workflow name + event. Key a check
  run that no workflow run made (an external App's) by name + App id.
- Drop a workflow run's check whose event is not `push`, `pull_request`, `pull_request_review`,
  `pull_request_target`, `deployment` or `deployment_status`: GitHub leaves it out of the pull
  request's checks ("Checks from some workflow jobs are not evaluated"), so it neither blocks nor
  waits.
- Two rows of one required name on a head are normal. A failed or cancelled newest run of one
  event blocks the merge beside a passing newest run of another event; a cancelled run that a
  newer run of the same event replaced blocks nothing.
- Check your rule against GitHub's own answer, the commit's or pull request's
  `statusCheckRollup.state` (GraphQL) and `gh pr checks`, which key the same way.

## Evidence

- Scratch sjawhar/legion#2250 at `672e3e1a`: review runs cancelled the `pull_request` `pr-title`
  run 40329132323 while review run 40330236878 passed. `gh pr checks` listed `pr-title fail` and
  `pr-title pass`, and the rollup stayed `FAILURE` with lint, typecheck and test green.
- Scratch #2255 at `88e0dffe`: several cancelled `pr-title` runs, the newest run of each event
  passed, rollup `SUCCESS`; an any-red rule called it failure.
- sjawhar/legion#2234 at `44eab33b`: a queued `pull_request_review_comment` `pr-title` run on the
  head, rollup `SUCCESS`.
- The rule as implemented in `packages/dispatch/web/src/features/issue/GitHubLink.tsx`
  (`newestCheckRuns`) matched GitHub's rollup on the 60 newest sjawhar/legion pull requests; the
  any-red rule matched 40.

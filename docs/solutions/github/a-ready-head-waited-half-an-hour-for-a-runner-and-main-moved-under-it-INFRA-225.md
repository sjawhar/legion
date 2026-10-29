---
title: "A READY head waited half an hour for a runner, and main moved under it"
category: github
tags:
  - ci
  - runner-capacity
  - required-checks
  - merge-conflict
  - queue-time
date: 2026-10-26
status: active
module: .github/workflows
related_issues:
  - "INFRA-225"
  - "sjawhar/legion#2188"
  - "sjawhar/legion#2173"
---

# A READY head waited half an hour for a runner, and main moved under it

On `sjawhar/legion`, the time between READY and the merge is mostly time waiting for a GitHub
runner, not time running checks. While a READY head waits, `main` keeps moving, and every
conflict that arrives in that window costs a whole round: a forward merge, a test round, a review
round, a retro and a second READY.

- When you read why a READY head has not merged, read the job timings
  (`gh api repos/<owner>/<repo>/actions/runs/<id>/jobs --jq '.jobs[] | {name, created_at, started_at, completed_at}'`),
  not just the run's state. A `queued` job with an old `created_at` means the runners are full.
  It does not mean the run is slow.
- Count runner wait as its own cost when you plan capacity: here a job that runs for 11 seconds
  waited 29 minutes for a runner.
- These measurements are evidence for runner-capacity work. Neither a Legion role nor a change in
  this repository can shorten the wait.

## Evidence

The READY head was d5530b01 (code identical to the approved head). The architect records READY
accepted at 08:16Z. The pull-request workflow runs were all created at 08:14:43Z on 2026-10-26, and
each job's `created_at` is 08:14:44Z:

| Job (run) | Started | Waited for a runner | Ran |
| --- | --- | --- | --- |
| `pr-title` (PR Title 40290914513) | 08:44:12Z | 29 min 28 s | 11 s |
| `test` (Tests 40290914446) | 08:47:16Z | 32 min 32 s | 18 min 38 s |
| `lint` (Tests) | 08:48:44Z | 34 min 0 s | 3 min 36 s |
| `typecheck` (Tests) | 08:50:21Z | 35 min 37 s | 32 s |

`main` merged sjawhar/legion#2173 at 08:43:49Z, before any of the four required jobs had started.
That merge took this change's contract and migration numbers, so the head conflicted. GitHub then
ran nothing on it, and the issue went back to `implementing` for a renumbering forward merge
(680616ac). That cost one more implementing, test, review and retro round. All four jobs ended
`success`, after the head had already conflicted.

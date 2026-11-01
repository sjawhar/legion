---
title: "A regression lock binds only as far as its lane is required: read the base's required set before you promise a rule cannot be undone"
category: testing
tags:
  - required-checks
  - regression-lock
  - merge-gate
  - ci
date: 2026-10-24
status: active
module: legion-worker
related_issues:
  - "INFRA-149"
  - "sjawhar/legion#2143"
---

# A regression lock binds only as far as its lane is required

When a review asks you to pin a rule so it cannot be undone silently, the test is half the answer.
The other half is which job runs it and whether the base branch requires that job.

- **Read the required set before you promise anything.**
  `gh api repos/<o>/<r>/rules/branches/<base> --jq '[.[] | select(.type=="required_status_checks")] | .[].parameters.required_status_checks'`,
  plus `.protection.required_status_checks` when the base has branch protection. A job outside that
  list is advisory: GitHub will merge over its red, and Legion's daemon sends work back only for
  "a check or workflow the base branch requires", so an advisory red stops nothing by itself.
- **Say which of the two you built.** "An edit that undoes this fails a lane" and "an edit that
  undoes this cannot land" are different promises. Write the one that is true in the pull request
  body and the handoff; a reviewer reading "fails a check" will hear the stronger one.
- **Put the pin where its neighbours are anyway.** A rule already pinned by a suite belongs in
  that suite, even when the suite is advisory: two pins of one fact in two lanes make a single
  legitimate edit fail twice and teach the next worker to weaken whichever one is louder.
- **Where the stronger promise is needed, the change is a repository setting, not a test.** Adding
  the job to the base's required set is an administrator action on the ruleset; raise it rather
  than writing a second test and calling the rule locked.

## Evidence

On sjawhar/legion#2143 a review round required a lock on the one sentence the change exists to
state. It was added to `packages/pi-legion/src/skills-guard.test.ts`, beside the existing pin of
the `skip-checks` recipe's shape, and proven to discriminate: with the sentence replaced in one of
the four files the test alone went red, `found: 0` against the expected `found: 1`, naming the
file.

Read on 2026-10-24, main's rulesets require exactly `lint`, `typecheck`, `test` and `pr-title`,
and branch protection adds none. That suite runs in the `pi-legion` job of
`.github/workflows/envoy-and-contracts.yaml`, which is not one of the four; the required `test`
job of `.github/workflows/pr-and-main.yaml` runs the release and check-script suites, the Go
daemon's tests and `scripts/e2e/lib`, not the plugin packages. So the lock turns a lane red and is
visible on the pull request, and it does not by itself block a merge — the same is true of every
existing pin in that file, which is why the placement was still right.

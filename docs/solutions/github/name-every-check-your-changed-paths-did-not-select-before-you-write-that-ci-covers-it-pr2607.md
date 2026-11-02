---
title: "Name every check your changed paths did not select, before you write that CI covers it"
category: github
tags:
  - ci
  - path-filters
  - evidence
date: 2026-10-31
status: active
module: .github/workflows
related_issues:
  - "sjawhar/legion#2607"
---
# Name every check your changed paths did not select, before you write that CI covers it

Extends docs/solutions/legion/a-red-lane-outside-the-required-set-merges-unread-read-every-lane-that-builds-the-changed-package-INFRA-379.md, which is about lanes that ran and went unread.
This is the lanes that never started, which no read of the head's check runs can show you, because they are not there.

- On a path-filtered router, the set of jobs a pull request runs is a function of the paths it touches.
  "CI passes" is therefore a statement about the job set your diff chose, and a change shaped as a deletion chooses a smaller one.
- Before an acceptance criterion or a pull request body says CI covers a check, read each filter against your changed paths and list the jobs the diff selects.
  Name every check you rely on that is not among them, run it at the head yourself, and paste its output beside the CI rows.
- Never let an incidental file in the diff be the reason a check ran.
  When the check matters, say which path selects it; when nothing in the change selects it, that check is yours to run, every round.

## Evidence

- INFRA-909's spec required six checks green on the head.
  On its original three-file path set, measured in planning, `ci.yaml`'s `test` job and its `ci-filters` job were both unselected, so the four `.github/scripts/*.test.sh` files and `check-ci-filters.sh` would not have run in CI at all.
- One file put in scope for an unrelated reason changed that: `.github/scripts/check-release-credentials.sh` carried a stale sentence ("four build jobs"), and the `test` filter names `.github/scripts/*` (`.github/workflows/ci.yaml:473`), so the four `.test.sh` files ran.
  That is the accident this rule exists to stop relying on; the next change may have no such file.
- `check-ci-filters.sh` stayed unselected: the `ci_filters` filter names `ci.yaml`, the check's own four scripts, the target lists and Go, none of which that change touched.
  It was run at the head instead, 3m38s, exit 0, and its last two lines went into the pull request body's check table under an explicit "not selected by CI here" row, with the tester reproducing it.
- The pull request body's table names, for each of the six checks, whether its result is a CI job on that pull request or a local run, so a reader never has to infer which.

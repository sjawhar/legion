---
title: "A sjawhar/legion pull request opens with a conventional commit title"
category: github
tags:
  - pr-title
  - required-checks
  - conventional-commits
date: 2026-10-27
status: active
module: .github/workflows
related_issues:
  - "INFRA-530"
  - "sjawhar/legion#2307"
---

# A sjawhar/legion pull request opens with a conventional commit title

- **Give `gh pr create --title` a subject of the form `type(scope): summary`.** The pattern is
  `^[a-z]+(\([^)]+\))?!?: .+` (`.github/workflows/pr-title.yaml:71`). For example:
  `fix(dispatch): store an email address written into a document as a mailto: link`.
- The `pr-title` job is a required check of main's ruleset. A plain-sentence title fails that
  check, and the tester records the red check as a failure. The squash merge uses the title as
  main's commit subject, and the release workflows set the version bump from its type
  (docs/solutions/legion/worker-pane-shell-gotchas.md, "only the PR title's conventional-commit
  type classifies the bump").
- `gh pr edit <n> --title '…'` re-runs only the PR Title workflow, not Tests.

## Evidence

#2307 opened as "Dispatch stores an email address written into a document as a mailto: link".
`pr-title` failed at 8a431a76 (run 40434368969: `PR title must be a conventional commit
subject`). The tester's round failed on that check and on a conflict with main
(`.legion/INFRA-530/test.json`, removed before the merge). This cost one test round and one
implementing round. Neither the PR body template (`skills/legion-worker/references/pr-body.md`)
nor the plan said what the title must look like.

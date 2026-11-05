---
title: "A workflow asks whether a run is on its own repository by id, never by name"
category: github
tags:
  - github-actions
  - fork-gate
  - rename
date: 2026-11-04
status: active
module: .github
related_issues:
  - "cybermonklabs/legion#2734"
---

# A workflow asks whether a run is on its own repository by id, never by name

- In any workflow condition or script that asks "is this run on this repository, rather than a
  fork", compare `github.event.pull_request.head.repo.id` with `github.repository_id`.
- Never compare a payload name with a context name. A payload field holds what was true when the
  event was created; `github.repository` is read now; and a re-run executes the workflow files of
  **its own commit**. After a transfer or a rename the two differ for the repository's own branch,
  so every gated step does nothing and says nothing.
- An id survives a rename and a fork's id still differs, so the gate keeps the property it was
  written for.
- When a fix of this class cannot be exercised before the merge — because the defect's own
  mechanism forbids it — write that in the pull request body with the measured defect beside it,
  rather than leaving the line looking unproven by neglect. A re-run of a pre-change run executes
  the old comparison; a run whose own commit carries the fix exists only after the merge.

## Evidence

cybermonklabs/legion#2734, commit `d63ae736`. Four steps decided "not a fork" by comparing
`github.event.pull_request.head.repo.full_name` with `github.repository`: the three steps of
`.github/actions/ci-ok-report/action.yml` (now `:33`, `:40`, `:49`, with the reason in its
description block at `:11-16`) and `.github/workflows/ci.yaml:1382`, the step that posts
`pr-title` on the head.

The repository was transferred from the `sjawhar` owner to `cybermonklabs` on 2026-11-03.
Measured on pull request 2671, run 41671895107 attempt 2: 31 jobs passed and no `ci-ok` was
posted, because the payload still said the old name while `github.repository` said the new one, so
every job's report step was skipped and the required check was never created.

`git grep full_name` over `.github` finds no comparison left at `6a0d666e17d9`.

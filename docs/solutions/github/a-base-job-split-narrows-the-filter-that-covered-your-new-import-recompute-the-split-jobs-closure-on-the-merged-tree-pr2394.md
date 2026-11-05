---
title: "A base job split narrows the filter that covered your new import: recompute the split job's closure on the merged tree"
category: github
tags:
  - ci
  - path-filter
  - forward-merge
  - check-ci-filters
date: 2026-10-31
status: active
module: .github/workflows
---

# A base job split narrows the filter that covered your new import: recompute the split job's closure on the merged tree

Extends docs/solutions/github/a-branch-that-deletes-files-main-still-edits-ports-each-edit-at-every-forward-merge-and-reruns-its-coverage-checks-on-the-merged-tree-pr2316.md,
for the case where main splits a job out of a broad filter.

## The rule

- When a forward merge brings a job split that moves your package out of a broad filter (such as
  `packages/envoy/**`) into a filter that lists packages one by one, that new list is the closure
  of main's tree. It does not include the packages your branch made your package import. The broad
  pattern used to cover them, so nothing told you.
- After such a merge, run `.github/scripts/check-ci-filters.sh` on the merged tree, and add every
  file it names to the split job's filter.
- Your branch's own filter lines go under the job that now compiles the package that needs them,
  whatever place the text merge left them in.
- Neither parent shows the gap: your head is green because the broad pattern covered the import,
  and main's head is green because main's copy of the package has no such import. The gap exists
  only in the merged tree.

## Evidence

- Merge `6b29d18f` of main `e018f1d3` in sjawhar/legion#2394 brought main's split of the race job.
  `envoy_go_race` keeps `'packages/envoy/**'`, and the new `envoy_go_race_pmdoc` lists packages
  (`.github/workflows/ci.yaml`).
- This pull request makes pmdoc import `internal/dispatch/text` (`pmdoc/pointers.go`,
  `pmdoc/schema.go`) and `internal/dispatch/model` (`pmdoc/schema.go`). Main's pmdoc imports
  neither, so `check-ci-filters.sh` passes at main's head.
- On the merged tree it reported `envoy-go-race-pmdoc: 431 files, 14 uncovered`, all under
  `model/` and `text/`. The fix added `model/*`, `text/*` and `text/testdata/**` to
  `envoy_go_race_pmdoc`, and moved this pull request's `packages/contracts/src/**` line there.
  The check then exits 0.
- For contrast, `envoy_go_race_docs` already named `model/*` and `text/*`, because main's `docs`
  package imports both.

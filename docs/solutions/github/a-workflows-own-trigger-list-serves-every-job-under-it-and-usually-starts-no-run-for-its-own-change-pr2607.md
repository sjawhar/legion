---
title: "A workflow's own trigger list serves every job under it, and usually starts no run for a change to itself"
category: github
tags:
  - path-filters
  - workflow-triggers
  - acceptance-criteria
date: 2026-10-31
status: active
module: .github/workflows
related_issues:
  - "sjawhar/legion#2607"
---
# A workflow's own trigger list serves every job under it, and usually starts no run for a change to itself

Extends docs/solutions/github/the-filter-drift-gate-is-one-sided-a-filter-wider-than-its-closure-passes-pr2516.md, with docs/solutions/github/a-filter-pattern-that-matches-no-tracked-file-passes-every-drift-check-and-selects-nothing-pr2517.md as its sibling.
Those two are blind spots inside what the drift gate reads.
This is the third: the workflows it does not read at all, and the deletion direction.

- An entry in a workflow's `on.push.paths` serves every job in that workflow, not the job it was added for.
  Before you delete one, match the remaining globs against every remaining job's declared inputs and require zero uncovered.
  When you delete the job an entry was added for, the entry may still be owed by another build in the same workflow: check before you take it with the job.
- Check whether any gate holds that workflow's trigger to anything at all.
  Adding a job to a path-filtered router is usually gated; a workflow's own `on.push.paths` often is not, and a one-sided drift gate over the router says nothing about it.
- A workflow's trigger list usually does not name its own file, its scripts or the guides it reads, so a change to the workflow starts no run of it on merge.
  An acceptance criterion that says "the first run of X after the merge" then has no event, no deadline and no owner.
  Restate it as *the first run whose head has the merge commit as an ancestor*, name the event that will produce one, and record the baseline where a reader can reach it after the phase handoffs are deleted — the pull request body, not `.legion/`.

## Evidence (sjawhar/legion#2607, read at that head on 2026-10-31)

- The deletion direction, measured: `release.yaml`'s `on.push.paths` was matched against the read list of every remaining build job — the words after `--` in `PI_ENVOY_VERSION_ARGS`, `PI_LEGION_VERSION_ARGS` and `LEGION_VERSION_ARGS`, which is what `release-push.sh version` reads to decide whether a package releases.
  All 24 distinct read paths covered, `uncovered=0`.
  Control: the same probe with `packages/pi-envoy/**` deleted from the trigger reports `packages/pi-envoy/ UNCOVERED (PI_ENVOY_VERSION_ARGS)`.
- The entry still owed by another build: `packages/envoy-plugin/package.json` stays in the trigger although the job it served is gone, because `packages/daemon/docker/worker.Dockerfile:175` copies that file into the worker image and `.github/scripts/check-image-trigger-paths.sh` requires the trigger to cover every input an image build reads.
  The other pull request deletes both trigger lines; this one keeps that one, and the two deletions are ten lines apart, so the regions do not overlap.
- The gate's boundary, read in the source rather than inferred: `.github/scripts/ci_job_reads.py:369` is `PUSH_WORKFLOWS = {"docs": ".github/workflows/docs-pages.yaml"}`, and `check_push_workflows` in `ci_filter_coverage.py:820-833` is the only place a workflow's own `on.push.paths` is held to anything.
  `release.yaml` is in neither, so `check-ci-filters.sh` would not have gone red for a trigger entry deleted out from under a remaining build job.
  The probe above was a throwaway script; the durable fix is a rule that reads `release.yaml`'s trigger, which is a repository change rather than a learning.
- The missing event: `release.yaml`'s `on.push.paths` runs from `"package.json"` to `".github/workflows/worker-image.yaml"` and names no `.github/workflows/release.yaml`, no `AGENTS.md` and no `.github/scripts/**`.
  It lists a sibling workflow file and not its own, so a pull request that changes the release workflow starts no release run when it merges.
  The spec's criterion named "the first `release.yaml` run on `main` after the merge"; planning restated it as the earliest run whose head has the merge commit as an ancestor, named the other pull request's merge as what will produce one, and put the tag-set baseline in the pull request body, which survives the removal of the handoff directory.

---
title: "Measure a build cache by what a new commit invalidates, and a hit by single-job re-runs of the run that wrote it"
category: github
tags:
  - buildkit
  - build-cache
  - github-actions
  - actions-cache
  - measurement
  - rerun
date: 2026-10-28
status: active
module: .github/workflows
related_issues:
  - "sjawhar/legion#2297"
---

# Measure a build cache by what a new commit invalidates, and a hit by single-job re-runs of the run that wrote it

Extends docs/solutions/daemon/a-file-built-from-the-final-images-own-state-is-a-build-only-stage-from-a-named-base-INFRA-255.md.

1. Before you add or keep a BuildKit layer cache for a workflow on `main`, read the Dockerfile
   for what changes on every commit: a build argument filled from `github.sha` (or any per-run
   value) makes every RUN below it a miss on every push. With `mode=min` only the final stage's
   layers are exported, so an intermediate stage that holds such a RUN, and every stage it copies
   from, runs again in full. Then the cache helps only a re-run at the same commit.
2. A hit measured on a re-run at the same commit is the best case. It never shows what a push to
   `main` gets. State the cold (no cache) time as the expected time unless step 1 shows the
   stages a typical commit leaves unchanged.
3. To measure a hit before the merge when only `main` writes the cache, let the branch write its
   own scope in a temporary commit (revert it before the final head), then re-run single jobs of
   that same run right after they wrote
   (`gh api -X POST repos/<owner>/<repo>/actions/jobs/<job-id>/rerun`; cancel the downstream join
   job before the next re-run, since a run in progress refuses one). Do not rely on a later
   dispatch: it can wait hours for a runner, and a repository Actions cache over GitHub's 10 GB
   limit evicts a branch's entries meanwhile, so the dispatch measures a miss.
4. Read `gh api repos/<owner>/<repo>/actions/cache/usage` before you plan a measurement. Over
   10 GB, eviction is already running.

## Evidence

- `packages/envoy/docker/Dockerfile:76-84` passes `LEGION_COMMIT` (the run's `github.sha`) into
  the `go build -ldflags` RUN of the `build` stage, which copies from the `web` stage
  (`Dockerfile:56,62`). With round 2's `type=gha,scope=envoy-<arch>,mode=min` cache, a push to
  `main` would have rebuilt both stages every time, so the pull request removed the cache.
- Same-commit re-runs hit every step (41 `CACHED`): amd64 2.00 min and arm64 1.60 min, jobs
  122722922008 and 122716488972 of run 40521465193 (temporary commit `d2605357`, reverted by
  `99da66ef`). The cold legs took 4.2–5.3 min (runs 40546893651 and 40666030478).
- The repository's Actions cache held 12.06 GB on 2026-10-28, and its oldest entry was under 18
  hours old. The branch's entries, written at 16:30 and 16:56 UTC, were gone when dispatch run
  40546893651 started its legs at 20:36 UTC, so that run measured a miss.

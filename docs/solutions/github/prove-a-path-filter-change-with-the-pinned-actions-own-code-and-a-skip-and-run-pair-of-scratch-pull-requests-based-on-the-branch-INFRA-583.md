---
title: "Prove a path-filter change with the pinned action's own code and a skip-and-run pair of scratch pull requests based on the branch"
category: github
tags:
  - paths-filter
  - dorny-paths-filter
  - github-actions
  - scratch-pull-request
  - negative-control
  - runner-queue
date: 2026-10-27
status: active
module: .github/workflows
related_issues:
  - "INFRA-583"
  - "sjawhar/legion#2325"
---

# Prove a path-filter change with the pinned action's own code and a pair of scratch pull requests

Extends docs/solutions/testing/a-throwaway-sandbox-repository-probes-workflow-semantics-with-a-control-job-and-every-production-first-step-rides-the-same-run-and-a-reimplementation-is-differentialled-against-its-oracle-first-LEGION-748.md
for dorny/paths-filter.

- **Run the pinned action's own `dist/index.js` as the oracle.** Fetch it at the pinned SHA
  (`https://raw.githubusercontent.com/dorny/paths-filter/<sha>/dist/index.js`; `gh api` refuses an
  owner outside the project). In a scratch git repository under `$TMPDIR`, commit the probe paths,
  modify one per run, and run it with `base: HEAD`, which diffs the working tree and needs no
  token or remote:

  ```bash
  echo '{}' > event.json   # GITHUB_EVENT_PATH must be a JSON file
  env -i PATH="$PATH" HOME="$TMPDIR" GITHUB_OUTPUT="$out" GITHUB_EVENT_NAME=push \
    GITHUB_EVENT_PATH="$PWD/../event.json" GITHUB_WORKSPACE="$PWD" INPUT_BASE=HEAD \
    INPUT_FILTERS="$(cat filters.yaml)" INPUT_LIST-FILES=none INPUT_PREDICATE-QUANTIFIER=some \
    node ../dist/index.js
  grep -o 'Changes output set to .*' "$out.log"   # the filters that matched
  ```

  Extract `filters.yaml` from the edited workflow with a YAML parser (the `with.filters` string of
  the step), never from a copy. Its negative control: an undefined anchor makes the action exit 1
  and set no output. This also proves nested anchors (`- *group` inside an anchored group) flatten.
- **Open scratch pull requests based on the issue branch**, not on main, so the run uses the new
  workflow and the file list is only the probe: one change every filter must skip, and one control
  change every job must run on. Read the verdict from `gh api repos/<o>/<r>/commits/<sha>/check-runs`
  (`conclusion: skipped` with no runner versus a started job), record the run URLs, then close the
  pull requests and delete their branches.
- **Do not wait on a congested runner pool for a filter-only change.** When the matcher's result is
  identical for every tracked path (compare the old and new filters over `jj file list` with the
  same matcher), the earlier scratch runs still hold, and the pinned-action run is the proof;
  cancel a scratch run that has not started.
- **A re-run cannot clear a red the base caused.** A `pull_request` re-run reuses the merge commit
  of its original event, so a failure from a base bug that main fixed later fails again; only a new
  event (a push, or a forward merge the architect authorizes) builds a fresh merge commit.

## Evidence

- Scratch #2326 (a comment in `packages/contracts/src/index.ts` and a blank line in
  `skills/dispatch/SKILL.md`), run 40447824879 attempt 1: the five Envoy Go jobs `skipped`.
  Scratch #2327 (a comment in `packages/envoy/internal/pgmigrate/scan.go`), run 40447830920: all
  five ran and ended `success`.
- Review round 1 de-duplicated the anchors and nested `dispatch_server` inside
  `dispatch_command_tests` (d63b45da). dorny/paths-filter@ceb8a2b8 `dist/index.js` on c53d0f10's
  filters started dispatch_memory on `internal/pglisten`, which it names only through the nested
  anchor; the same filters with an undefined anchor exited 1. Scratch #2386 for the same probe was
  cancelled unstarted with over 200 runs queued.
- `test` was red at eb8c3209 (run 40448233739, started 04:10Z on 2026-10-27) on a daemon read that
  #2317 fixed on main at about 04:19Z; this pull request touched no daemon code. The architect
  authorized the forward merge 9ec4eb64, whose diff fingerprint was unchanged.

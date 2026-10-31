---
title: "A filter pattern that matches no tracked file passes every drift check and selects nothing"
category: github
tags:
  - ci-router
  - path-filters
  - drift-checks
date: 2026-10-30
status: active
module: .github/scripts
related_issues:
  - "sjawhar/legion#2517"
  - "sjawhar/legion#2595"
---

# A filter pattern that matches no tracked file passes every drift check and selects nothing

Extends `docs/solutions/github/the-filter-drift-gate-is-one-sided-a-filter-wider-than-its-closure-passes-pr2516.md`,
which says a filter wider than its job's closure passes. This is the other blind spot, and it is
the one a reader cannot see.

**A check that holds a filter to a table of declared reads passes a pair that is wrong in the same
way.** Both lists naming a path that does not exist agree with each other: the declared read
expands to no file, so there is no file to hold the filter to, and every rule of the check is
satisfied while a change to the files they meant starts no job — on the pull request and again on
main, where nothing re-runs it.

**So hold every pattern to the tree as well as to the other list.** Expand each filter pattern and
each declared read over the repository's own file set, with the matcher the check already uses,
and fail any that matches nothing, naming the pattern and the filter, gate or table row it is in.
Exempt a `!` pattern: it names what to leave out, and leaving nothing out is no fault.

**Stale line citations beside a path are the tell.** A row whose cited line numbers no longer
exist in the file it names has not been re-read in a long time; treat the path in it as suspect.

**Cover each branch of the new rule with its own case**, including the exemption: a dead read in a
gate, a dead pattern in a filter, a dead read in a per-source rules row, and a `!` pattern that
matches nothing and must still pass.

**A pattern the rule fails may be a pattern that should never have been there.** Before you teach
the rule an exemption for a path that is empty today, ask whether the job reads those files at all.
A build reads what its context holds, so the honest place to say "no build reads this" is the
ignore file the build already has, and the filter then leaves the files out too.

## Evidence (sjawhar/legion#2517)

`.github/workflows/ci.yaml` and `.github/scripts/ci_job_reads.py` named
`packages/contracts/fixtures/delivery-live/**`. The directory is
`packages/contracts/fixtures/live/` (14 files), which
`internal/dispatch/api/delivery_live_diff_test.go` names in its own text. The dead path was in
four filters — `dispatch`, `dispatch_browser_multiprocess`, `envoy_go` and `envoy_go_race_api` —
over three literal lines, one of them a shared anchor, and in two gate rows, whose citations
pointed at `delivery_live_diff_test.go:965` where the reads are at `:982` and `:1011`. A change
touching only those fixtures started none of those four jobs, and `check-ci-filters.sh` had been
green over it for as long as it had been wrong.

Running the new rule over the whole tree found a second one that was not a rename: a rules row for
`packages/daemon/internal/claudebridge/bridge_test.go` declared
`packages/pi-legion/src/phase-stall.ts`, a file that moved to `packages/pi-shared/`, and the row's
other reads cited lines `:816-817` of a 587-line file with no `bun` call and no `phase-stall`
reference at all. The read it described no longer happened, so the row went rather than being
repointed.

The rule is `check_dead_patterns` in `.github/scripts/ci_filter_coverage.py`, counted into the
check's own failure tally, with its cases at `.github/scripts/check-ci-filters.test.sh:263-295`.

## The exemption that was not needed (sjawhar/legion#2595)

The rule went green on main and then failed there. The fold commit f4ec92ee5, "chore: fold
changelog fragments", folded every `packages/envoy` changelog fragment into its `CHANGELOG.md` and
removed the last file under `packages/envoy/.changes/`, so the `worker_image` filter's
`packages/envoy/.changes/**` matched nothing. The rule failed every later pull request through
`ci-ok`, a required check, and the fold's own `[skip ci]` meant main never ran the check the fold
had just broken.

The pattern could not simply be dropped while the fragments were in the image's build context:
`check-image-trigger-paths.sh` fails `worker_image` the moment a fragment exists without a pattern
covering it, because `packages/daemon/docker/worker.Dockerfile` copies seven packages that hold
fragments. Two checks held one line to opposite verdicts, depending on whether a release had
shipped since the last fragment was written.

Neither check was wrong. The filter was: a changelog fragment is not an image input, and
`packages/claude-envoy/scripts/build-inputs.txt` already said so. `.dockerignore` now leaves
`packages/*/.changes/` out of the context, as it already does for `packages/envoy/infra/`, and
`worker_image` carries `!packages/*/.changes/**`. The rule needed no exemption, a pull request that
only writes a changelog entry stops rebuilding the worker image, and the two checks hold the pair
together: removing the `.dockerignore` line while the filter keeps the exclusion makes
`check-image-trigger-paths.sh` fail, naming an uncovered fragment for each copied source that would
then hold one and counting the others.


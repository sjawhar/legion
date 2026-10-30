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
  - "INFRA-757"
  - "sjawhar/legion#2517"
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

## Evidence (INFRA-757, sjawhar/legion#2517)

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

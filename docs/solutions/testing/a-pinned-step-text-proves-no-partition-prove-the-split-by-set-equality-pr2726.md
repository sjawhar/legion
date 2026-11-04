---
title: "A pinned step text proves no partition: prove a two-step package split by set equality"
category: testing
tags:
  - ci
  - go-test
  - race-detector
  - ci-filters
  - packages-daemon
date: 2026-11-03
status: active
module: .github
related_issues:
  - "sjawhar/legion#2726"
---

# A pinned step text proves no partition: prove a two-step package split by set equality

Extends docs/solutions/testing/a-race-lane-counts-only-once-it-covers-the-new-goroutines-a-canary-fails-it-and-a-race-injected-into-the-covered-code-fails-it-INFRA-271.md.

That note's step 1 says where to edit when you add a package to the daemon's race lane
— `DAEMON_RACE_PACKAGES` in `.github/scripts/ci_job_reads.py` and both daemon steps of
`.github/workflows/ci.yaml` — and that `check-ci-filters.sh` then holds `ci.yaml` to that one
list.
It is easy to read that as a guarantee that every package still runs somewhere.
It is not one.

- **Hold both step texts to one list, then prove the split separately.**
  `check-ci-filters.sh` fails when a step text drifts from the list or a pinned script changes by
  a byte, and on eight other conditions; none of the nine asks whether every package of the module
  is reached by some step.
  The plain step's `exact=` entry declares the superset `./...` on purpose, so a package neither
  step selects is invisible to the checker.
- **The proof is set equality at the head, read from the job's own log**: take each step's package
  lines, check that no package appears in both, and compare their union with `go list ./...`,
  sorted.
  Record the three counts, so a later reader can see that they add up.
- **The exclusion is a `grep -E` over the import path's suffix**, which decides three cases the
  list does not.
  A future package at any `<dir>/internal/record`, `internal/daemon`, `internal/sessionproxy` or
  `internal/launcher` is dropped from the plain step and is not in the race step's literal list,
  so neither step runs it.
  A new `internal/daemon/sub` is **not** in the race lane although its parent is.
  A package name holding a regular-expression metacharacter widens the pattern silently.
- **Removing a package from the plain step removes its only unraced run.**
  Say so where you say "once per job", and name what still compiles it: `-race` changes scheduling
  and roughly triples wall time, so a failure that shows only without the detector, or one its
  slowdown masks, then has no lane at all.
- **Keep `set -o pipefail` on the pipeline, and know what it catches.**
  It fails the step when `go list` fails after printing part of its output, which would otherwise
  test a subset and pass, and when the selector selects nothing.
  Do not rely on an empty selection failing by itself: it does so here only because
  `packages/daemon`'s module root holds no Go files, so `go test` with no arguments resolves to
  `.` and exits 123.
  In a module whose root holds Go files, the same step would pass having tested one package.

Two mechanics of the checker, for whoever runs it:

- Print the pinned texts **from the repository root**:
  `PYTHONPATH=.github/scripts python3 -c 'import ci_job_reads as m; print(m.DAEMON_RACE_TEXT); print(m.DAEMON_PLAIN_SCRIPT)'`.
  The module does repository-root-relative I/O at import time, so `cd .github/scripts` first ends
  `FileNotFoundError: [Errno 2] No such file or directory: 'packages/envoy'`.
- Read the **first** problem, not the count.
  A pinned script one byte off its text stops being reduced, so its runs claim every Go file of
  the repository and the checker reports 22 problems for one edit.

## Evidence

sjawhar/legion#2726 (dispatch://INFRA-1052) stopped the `test` job running `packages/daemon`'s
four concurrent packages twice.
The plain step became
`go list ./... | grep -Ev '/(internal/daemon|internal/record|internal/sessionproxy|internal/launcher)$' | xargs go test -count=1 -timeout 15m`
under `set -o pipefail`, and `DAEMON_RACE_PACKAGES` builds both pinned texts
(`.github/scripts/ci_job_reads.py:365-376`, `:414-421`; `.github/workflows/ci.yaml:1893-1898`).

- The checker's nine failure conditions are listed in `.github/scripts/check-ci-filters.sh`'s
  header; the partition is in none of them.
- The split was proven by set equality instead, in job 126135829243 of run 41698639673: the plain
  step printed 63 package lines, 59 `ok` and 4 `?`, none for the four; the race step printed
  exactly four `ok` lines; the 63 and the 4 sorted together are the 67 import paths
  `go list ./...` prints at that head.
- Three controls in a scratch clone, each changing `ci.yaml` alone: the plain step's
  `-timeout 15m` written as `20m` exits 1 with 22 problems, the first reading "the step's script
  is not the pinned text"; the race step with `./internal/launcher/` removed exits 1 with 2; the
  plain step without `set -o pipefail` exits 1 with 22.
- Measured in `packages/daemon` at that head: `go list ./...` prints 67 paths, the pattern selects
  63 and matches 4; with the pattern replaced by one that matches every package,
  `go list ./... | grep -Ev '.' | xargs -r go test …` exits 1 under `pipefail` and 0 without it,
  while the same pipeline with plain `xargs` exits 123 either way.
- The suffix-match case is recorded as risk F5 of the issue's plan and in the pull request's
  `Not proven / risk` line; no package of the module is in that shape today.

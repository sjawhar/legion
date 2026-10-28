#!/usr/bin/env bash
# ci.yaml's `changes` job selects each job through a dorny/paths-filter filter, and a change to a
# file a job reads but its filter misses skips that job, on the pull request and again on main,
# where nothing re-runs it. So every filter must match every file its job reads. For a Go job
# that set moves with every import, embed and test file, so this check recomputes it on every
# change to Go code, a module file or ci.yaml, and fails naming each file a filter misses.
#
# What a job reads is, per gate (.github/scripts/ci_job_reads.py, GATES):
#   * the closure of each go command the job runs, live: `go list -deps [-test] [-tags ...]` over
#     the command's packages, in its directory, with its GOOS/GOARCH/CGO_ENABLED and -overlay.
#     Each first-party package directory counts as `<dir>/*`; a test run adds its `testdata/**`;
#     every run adds each embedded file and, for a `<root>/...` pattern, every directory with .go
#     files under root (the go command reads their build constraints);
#   * what a test compiled into such a run does outside its package (RULES, keyed by source file
#     and applied only when go list says the file is compiled under the job's tags): the closure
#     of a go command it starts, and each file it reads through `..`;
#   * the module files of each go command's workspace: go.work, go.work.sum and every bound
#     module's go.mod and go.sum (`go env GOWORK`, `go work edit -json`);
#   * the static reads of the job's other steps: actions, the Bun install, Biome, scripts, fixtures.
# Those globs are expanded over the repository's files (tracked, and untracked ones git does not
# ignore, so an uncommitted new file counts and a build output does not) and each file is held to
# the gate's filter as dorny/paths-filter v4.0.3 reads it (workflow_paths.PathsFilter: `some`, or
# `some-with-excludes`, where a `!` pattern excludes), anchors flattened into the list that names
# them.
#
# The table cannot drift from ci.yaml silently. Fails on:
#   1. a file a gate reads that its filter does not match (up to 20 named per gate, with why),
#   2. a gate whose filter the changes job lacks, or whose job's (or step's) if: does not test it,
#   3. a job or step gated on a filter the table neither lists in GATES nor EXCLUDES,
#   4. a run: line of a ci.yaml job that starts a go command (test, vet, build, run, list,
#      install, generate) no Run of a gate on that job claims, or whose packages (its relative
#      package arguments) are not exactly the claiming Run's, since only the Run's are listed,
#   5. a Run whose pinned text its step or script no longer holds, a RULES source file that is
#      gone, or a why citing a step the job no longer has,
#   6. a go command that fails or a first-party package go list cannot load, since its closure
#      is then unknown, and a `!` pattern under `some` or a brace in a gate's filter,
#   7. a SELECTED_BY in ci-ok's step that is not exactly {job: the output its if: tests} for every
#      job ci-ok needs but changes and lint, or a PULL_REQUEST_ONLY that is not exactly the jobs
#      whose if: restricts to github.event_name == 'pull_request': ci-ok.sh fails a job that map
#      says changes selected unless it succeeded, so a wrong entry fails a job that was rightly
#      skipped or passes one a failed-only re-run left skipped.
#
# The go commands run with GOPROXY=off over an empty module cache: nothing is downloaded, no
# go.work.sum is rewritten, and a cold runner gives the answer a warm machine does. Third-party
# packages then fail to load, which loses no first-party package, since none of them imports
# one: no module outside this repository requires one of its modules (`go mod graph`), and all
# but twelve of its 185 packages are main packages or internal/ ones, which nothing outside the
# repository may import. The first-party sets matched a run over a full module cache file for
# file when this check was written.
#
# EXCLUDED leaves worker_image to check-image-trigger-paths.sh, which proves it covers every file
# its image build reads. envoy_smoke is a gate whose image inputs are that check's: this one holds
# it to the job's go commands and other step reads. The RULES and the static reads are kept by
# hand: this check proves the filters cover them, not that they are complete, so a change that
# makes a test read a new file through `..` or start a new go command adds it to ci_job_reads.py.
#
# Run from anywhere: .github/scripts/check-ci-filters.sh
# CI runs it in ci.yaml's ci-filters job, and its tests in ci.yaml's test job.
set -euo pipefail

cd "$(dirname "$0")/../.."

# The engine (ci_filter_coverage.py), its data (ci_job_reads.py) and the path model
# (workflow_paths.py) sit beside this script in the repository it checks. The engine is imported,
# not run as a script, so the data's `from ci_filter_coverage import ...` binds the same classes.
# PYTHONDONTWRITEBYTECODE keeps the imports from leaving a __pycache__ in the tree.
PYTHONDONTWRITEBYTECODE=1 PYTHONPATH=.github/scripts exec python3 -c \
  'import sys, ci_filter_coverage; sys.exit(ci_filter_coverage.main())'

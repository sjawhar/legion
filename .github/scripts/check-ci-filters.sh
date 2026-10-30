#!/usr/bin/env bash
# ci.yaml's `changes` job selects each job through a dorny/paths-filter filter, and a change to a
# file a job reads but its filter misses skips that job, on the pull request and again on main,
# where nothing re-runs it. So every filter must match every file its job reads. For a Go job
# that set moves with every import, embed and test file, so this check recomputes it on every
# change to Go code, a module file or ci.yaml, and fails naming each file a filter misses.
#
# What a job reads is, per gate (.github/scripts/ci_job_reads.py, GATES):
#   * the closure of each go command the job runs, live: `go list -deps [-test] [-tags ...] [-race]`
#     over the command's packages, in its directory, with its GOOS/GOARCH/CGO_ENABLED and -overlay.
#     Each first-party package directory counts as `<dir>/*`; a test run adds its `testdata/**`;
#     every run adds each embedded file and, for a `<root>/...` pattern, every directory with .go
#     files under root (the go command reads their build constraints);
#   * what a test compiled into such a run does outside its package (RULES, keyed by source file
#     and applied only when go list says the file is compiled under the job's tags): the closure
#     of a go command it starts, and each file it reads through `..`;
#   * the module files of each go command's workspace: go.work, go.work.sum and every bound
#     module's go.mod and go.sum (`go env GOWORK`, `go work edit -json`);
#   * for a claimed go command this check does not reduce (below), and for every Run of an exact
#     entry whose step no longer holds its pinned script, every Go file of the repository and
#     every file under a Go module's directory;
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
#      install, generate) no Run of a gate on that job claims and no exact entry's step holds, or
#      a claimed line in the simple form (below) whose packages, -tags, -race or mode (test, vet,
#      or build) are not exactly its Run's,
#   5. a Run whose pinned text its step or script no longer holds, an exact entry whose step the
#      job no longer has, a RULES source file that is gone, or a why citing a step the job no
#      longer has,
#   6. a go command that fails or a first-party package go list cannot load, since its closure
#      is then unknown, and a `!` pattern under `some` or a brace in a gate's filter,
#   7. a job whose if: tests an output of changes that ci-ok's needs or its SELECTED_BY does not
#      name, which leaves that job advisory once ci-ok is the one required check, a SELECTED_BY
#      in ci-ok's step that is not exactly {job: the output its if: tests} for every job ci-ok
#      needs but changes and lint, or a PULL_REQUEST_ONLY that is not exactly the jobs
#      whose if: restricts to github.event_name == 'pull_request': ci-ok.sh fails a job that map
#      says changes selected unless it succeeded, so a wrong entry fails a job that was rightly
#      skipped or passes one a failed-only re-run left skipped,
#   8. a workflow ci_job_reads.py's PUSH_WORKFLOWS pairs with a filter whose on.push.paths do not
#      name exactly that filter's patterns (anchors flattened), naming each missing and extra one,
#      or that is missing or has no on.push.paths: it runs on main the work the filter's job runs
#      on pull requests and merge groups (docs-pages.yaml, the docs filter), so the two lists
#      cannot drift,
#   9. a pattern of a changes filter, or a read ci_job_reads.py declares (a gate's or a RULES
#      row's), that matches no file of the repository, naming the pattern and the filter, gate or
#      row it is in: a filter and a declared read that name the same path which does not exist are
#      consistent with each other and select nothing, so every rule above passes while a change to
#      the files they mean starts no job. A `!` pattern is exempt, since it names what to leave out.
#
# A claimed go command is reduced only in its simple form, never parsed further: one line of
# `go test|vet|build`, then relative package paths and only -v, -race, -count=N, -timeout D,
# -buildvcs=false and one -tags T, that is its step's whole run: script (split at \n alone,
# stripped of spaces and tabs, comments and blank lines aside; a script holding any character
# outside printable ASCII, tab and \n is not simple), with no shell: on the step and no
# defaults.run.shell on the job or the workflow, no container: on the job, no name outside
# ENV_ALLOWED (test configuration go never reads; GO*, CGO_*, BASH_ENV, ENV, HOME,
# XDG_CONFIG_HOME and PATH are outside it) in the workflow's, job's or step's env, no step of the
# job writing $GITHUB_ENV or $GITHUB_PATH or running `go env -w`
# (each reaches every later go command unseen), and the step's working directory (its own, else
# the job's or workflow's defaults.run, else the root) the Run's. A -race line's closure is the
# one `go list -race` lists, so its Run says race too. A script_run is held exact instead: the
# script's line holding its text must be that text alone and uncontinued, under such an env,
# shell and container (the job's and every one of its steps') and such a job; the script's
# variables are the Run author's to read, and anything appended to that line falls to everything.
# A step whose go commands are in no such form can be an exact entry instead: it pins the step's
# whole run: script (compared as text after trailing whitespace, nothing parsed) and directory,
# and lists Runs that stand for what the script compiles. While the step holds exactly that, under
# such an env, shell, container and job, its go lines are claimed and the Runs' closures checked;
# any difference makes every one of its Runs fall to everything. Any other claimed command - an
# assignment such as GOFLAGS, another flag, a pipe, a quote, a substitution, a cd, another line in
# its step - counts as reading every Go file of the repository, every file under a Go module's
# directory (each directory holding a go.mod: testdata, embedded files and all) and every go.mod,
# go.sum, go.work and go.work.sum, and fails naming the command unless its filter selects all of
# them. So a cd, `go -C` or a step in another directory than the Run's can only widen what it
# reads. It fails wide, never narrow: a new evasion is answered by falling to everything, not by
# more parsing.
#
# What it reads, and what it does not: ci.yaml's own steps (their run: text, shell, working
# directory and env: blocks), the job's and workflow's env and defaults, and the scripts and steps
# the table pins. It does not read the environment a local action or a repository script writes
# (a composite action's own $GITHUB_ENV or $GITHUB_PATH, a script's export), nor what such a
# script runs beyond its pinned line. A pinned repository script is read the way Python splits
# lines, so a \r hidden inside one is outside what it reads too. It answers accidental drift: a
# new import, file or command that silently stops a job running on its own inputs. A committer
# who deliberately hides a command or a flag where this check does not read is the review's to
# catch.
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

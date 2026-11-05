#!/usr/bin/env bash
# ci.yaml's `changes` job selects each job through a dorny/paths-filter filter, and a change to a
# file a job reads but its filter misses skips that job, on the pull request and again on the
# merge group that lands it, after which nothing runs it. So every filter must match every file
# its job reads. For a Go job that set moves with every import, embed and test file, so this
# check recomputes it on every change to Go code, a module file or ci.yaml, and fails naming
# each file a filter misses.
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
#   7. a CI_OK_SELECTED_BY in ci.yaml's env that is not exactly {job: the output its if: tests}
#      for every job whose if: tests an output of changes, or a CI_OK_PULL_REQUEST_ONLY that is
#      not exactly the jobs whose if: restricts to github.event_name == 'pull_request': ci-ok.sh
#      fails a job that map says changes selected unless it succeeded, and ci-ok-report.sh waits
#      for exactly those jobs, so a wrong entry fails a job that was rightly skipped, passes one a
#      failed-only re-run left skipped, or posts ci-ok before a job reported; a job but changes
#      and those CI_OK_EXEMPT names whose last step is not the ci-ok-report step, or that calls a
#      workflow with no such step reporting for it, since a run that job ends never gets ci-ok;
#      a job CI_OK_EXEMPT names that does report to ci-ok, or that CI_OK_SELECTED_BY also maps,
#      or that is no job of the file at all; and a job named ci-ok or pr-title, which would post
#      a required check itself,
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
#  10. an `if:` of ci.yaml, a job's or a step's, that tests `github.event_name == 'push'` or
#      `github.event_name != 'push'`, or that compares `github.ref` with a single-quoted
#      `'refs/heads/...'` literal with `==` or `!=` (either operand first), naming the job, the
#      step and the expression: this workflow has no `push:` trigger, so such a condition gates
#      its job or step on what no run of it ever is, and the dead code reads as live - a step
#      behind `if: github.event_name == 'push'`, or a job behind `if: github.ref ==
#      'refs/heads/main'`, runs in no run of this workflow while its text says it runs on main.
#      A cache that must be filled on a push to main belongs in .github/workflows/ci-caches.yaml;
#      a job that must run at main's tip is reached by the workflow_dispatch the bypass check
#      (.github/workflows/main-bypass-check.yaml) sends. It refuses those two comparisons and
#      nothing else: `github.ref_name`, and `github.ref` outside such a comparison, are left
#      alone, since a merge_group run's github.ref really is a branch ref
#      (refs/heads/gh-readonly-queue/...) and a startsWith on it is live. As everywhere here, the
#      same test written as a function call is the review's to catch, not this check's.
#  11. a cache family .github/workflows/ci-caches.yaml saves whose `key:` or `path:` is not
#      exactly that of the matching actions/cache/restore of ci.yaml (the keys compared as text,
#      trailing whitespace aside, the paths as ordered lists of stripped lines), or that ci.yaml
#      restores nowhere, and an actions/cache/restore of ci-caches.yaml that is not
#      `lookup-only: true` or that carries restore-keys. ci-caches.yaml fills the entries ci.yaml
#      restores, and a key or a path that drifts apart makes every restore miss silently: nothing
#      fails, every job just rebuilds from nothing what the fill already built. A save is matched
#      to a restore by family, the key's text up to its first `${{`, which is the Go version the
#      entry is keyed on. A family ci.yaml restores that ci-caches.yaml does not save is no
#      failure: go-docs is saved by docs-pages.yaml, from the build job that workflow shares with
#      ci.yaml's docs job. Each restore of ci-caches.yaml is a lookup, which must download
#      nothing: the workflow asks only whether that exact key is already there, no step of it
#      reads a restored entry, and a prefix hit would answer for an older key and skip the build
#      that fills this one, leaving it empty.
#  12. a job of ci.yaml that a merge-group run can run and that CI_OK_EXEMPT does not name -
#      the jobs ci-ok waits for there, plus `changes` - with no follower cancel job of its own,
#      a follower that follows no such job or more than one, and a follower whose `if:` is not
#      the one text every follower carries. A follower names the job it follows in `needs:` and
#      nowhere else, since .github/actions/merge-group-cancel reads that id out of
#      `toJSON(needs)` to find the job's own report; a job with no follower leaves every group
#      built on this one running to its end when it fails, and a condition written out by hand
#      31 times drifts.
#  13. a job of ci.yaml whose `if:` lacks `(github.event_name != 'merge_group' ||
#      github.run_attempt == '1')`, or, for a job whose `if:` admits merge_group alone (the
#      cancel jobs, rule 12's followers among them), `github.run_attempt == '1'`: a group is
#      tested once, and `gh run rerun --failed` leaves out a `changes` that succeeded and hands
#      the attempt that job's outputs again, so a job gated on `needs.changes.outputs.<x>`
#      alone runs in full on a re-run attempt of a merge-group run and posts a ci-ok on a head
#      the queue has thrown away. Each job needs the one term, so rules 12 and 13 never ask one
#      job for two.
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

# The engine (ci_filter_coverage.py), its data (ci_job_reads.py), the rules of ci.yaml that read
# neither (ci_workflow_rules.py: rules 10, 11 and 13 above) and the path model
# (workflow_paths.py) sit beside this script in the repository it checks. The engine is imported,
# not run as a script, so the data's `from ci_filter_coverage import ...` binds the same classes.
# PYTHONDONTWRITEBYTECODE keeps the imports from leaving a __pycache__ in the tree.
PYTHONDONTWRITEBYTECODE=1 PYTHONPATH=.github/scripts exec python3 -c \
  'import sys, ci_filter_coverage; sys.exit(ci_filter_coverage.main())'

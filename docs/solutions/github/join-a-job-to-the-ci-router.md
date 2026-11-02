---
title: "Join a Job to the CI Router: A Filter That Names Every File the Job Reads"
category: github
tags:
  - github-actions
  - path-filters
  - ci-router
  - caching
  - concurrency
  - merge-queue
date: 2026-10-27
status: active
module: ci
related_issues:
  - "INFRA-520"
symptoms:
  - "a change merged without a job that reads one of its files, and nothing re-ran the job"
  - "a new job never runs on a pull request, or runs on every pull request"
  - "ci-ok fails on a draft pull request although no job failed"
  - "ci-ok fails naming a job changes selected that ended skipped, after a re-run of failed jobs"
  - "ci-ok fails saying it does not know what selects a job"
  - "a pull request run writes an Actions cache entry"
---

# Join a Job to the CI Router

`.github/workflows/ci.yaml` (name `CI`) is the one workflow every pull request, every merge-queue
group and every push to main runs. It has no `paths:`. Its first job, `changes`, reads the paths
the change touched and selects jobs through one dorny/paths-filter filter per job. Its last job,
`ci-ok`, condenses every job's result into one check. The deployment repository keeps its own
router guide for its own workflows; this page covers this repository only.

## The six places a job joins

1. **A filter in `changes`** that names every file the job reads (the method is below).
2. **An output of `changes`** for that filter: `<filter>: ${{ steps.filter.outputs.<filter> }}`.
3. **`needs: changes`** on the job, and `if: needs.changes.outputs.<filter> == 'true'`.
4. **The job in `ci-ok`'s `needs:`.** A job that is not there can fail and still let `ci-ok`
   pass.
5. **The job in `ci-ok`'s `SELECTED_BY`**, mapped to the output its `if:` tests
   (`"<job>": "<filter>"`), and in `PULL_REQUEST_ONLY` too when its `if:` also admits only
   `github.event_name == 'pull_request'`. A job that runs on no push gets its `changes` output
   set false on a push instead (`github.event_name != 'push' && steps.filter.outputs.<filter> ||
   'false'`), so `ci-ok` reads its skip there as unselected. `ci-ok` fails a needed job `SELECTED_BY` does not name, and
   `check-ci-filters.sh` fails when the map or a list is not exactly what the `if:`s test (the
   section on `ci-ok` below says why).
6. **A gate in `.github/scripts/ci_job_reads.py`** that says what the job reads, which
   `.github/scripts/check-ci-filters.sh` holds the filter to (the section on the check below).

A Go job also restores its caches the way the section on caches below says.

## ci-ok fails a selected job that did not succeed

`ci-ok` runs `.github/scripts/ci-ok.sh`. It passes only when `changes` succeeded, lint
succeeded (it runs on every change), every job `changes` selected succeeded, and every job it
did not select succeeded or was skipped. ci-images is one of the selected jobs: the `ci_images`
filter selects it, and `SELECTED_BY` maps it. A selected job that was skipped, cancelled or is
absent from `ci-ok`'s `needs:` fails it, and it prints every job's result and whether `changes`
selected it.

A skipped job reports success, so accepting any skip is not enough. When `changes` fails, every
job its filters gate is skipped. GitHub's "Re-run failed jobs" then re-runs `changes` and
`ci-ok`, and `changes` can succeed while the jobs it now selects stay skipped: a `ci-ok` that
accepted those skips would pass with nothing tested. So `ci-ok.sh` decides what `changes`
selected from its outputs, through `SELECTED_BY`: a job is selected when the output it maps to is
`'true'`, and for a job in `PULL_REQUEST_ONLY` the run must also be a pull request's (docs,
whose main build is docs-pages.yaml's, has its output set false on a push). A re-run that
leaves a selected job skipped fails naming the job; re-run the whole workflow instead.

`check-ci-filters.sh` fails when `SELECTED_BY` is not exactly {job: the output its `if:` tests}
for every job in `ci-ok`'s `needs:` but `changes` and lint, when `PULL_REQUEST_ONLY` is not
exactly the jobs whose `if:` restricts to `github.event_name == 'pull_request'`. A wrong
entry would fail a job its `if:` rightly skipped, or pass one a re-run left skipped.

A job whose main half must run in push order, as the docs site's Pages deploy must, cannot be a
job of the router: each push to main is a router run of its own, and a job-level concurrency group
takes jobs as their needs end, not in push order. Its main half is a push-only workflow of its own
with a workflow-level group (`queue: max`, `cancel-in-progress: false`), whose `on.push.paths`
equals the job's filter; the router job admits no push (its `changes` output is false on a push),
and `ci_job_reads.py`'s `PUSH_WORKFLOWS` pairs the filter with that workflow, so
`check-ci-filters.sh` fails naming each pattern one list has and the other lacks.
docs-pages.yaml is the one such workflow.

## Build the filter by the method, not by guessing

For a Go job, list what its go commands compile:

```bash
go list -deps -test [-tags <tags>] -f '{{.Dir}}' <the packages the job's go commands name>
```

- Name each first-party directory as `<dir>/*` (its own files; a subpackage is listed on its own
  line), and add its `testdata/**` and every file it embeds.
- Add the closure of every go command a test starts (`go build`, `go run`, `go test` of another
  package): run the same `go list` over those targets.
- Add every file a test reads by walking up to the repository root (an `AGENTS.md`, a workflow,
  a fixture outside the package).
- Add the workspace's module files: `go.work`, `go.work.sum`, and the `go.mod` and `go.sum` of
  every module it binds (the `go_workspace` anchor), since they move the versions the build
  compiles.

For every job, add:

- the composite actions it uses (`.github/actions/<name>/**`) and what they read;
- the Bun install set when the job runs `bun install` (the `bun_install` anchor);
- Biome's files when it runs `bunx biome` (the `biome` anchor);
- `.github/workflows/ci.yaml` itself, since a job reads its own definition.

Write no `!` pattern. The step's quantifier is `some-with-excludes`: a change matches a filter
when one of its patterns without `!` matches and none of its `!` patterns does. A filter with no
`!` pattern therefore means what it would under the default, `some` (any pattern matches), and
every filter but `worker_image` must stay that way.

`worker_image` is the one exception. Its `!` patterns are exclusions under `some-with-excludes`:
after the roots the worker image build reads come one `!` pattern for each thing the
`.dockerignore` keeps out of the build context inside those roots. The check fails when
`worker_image` misses a file the build reads, and, since it gates a job that only builds, when it
matches a file the build does not read (the two workflow files aside): a pull request touching
only such files would build an image for nothing. When the Dockerfile copies from a new root, add
the root; when the `.dockerignore` drops something new inside a root, add its `!` pattern.

`.github/scripts/check-image-trigger-paths.sh` reads the image gates (`worker_image`,
`envoy_smoke`) only as flat lists of globs, so those two name no anchor. In an image gate it
refuses a `!` pattern under `some`, where it would match every file outside it, any quantifier but
`some` and `some-with-excludes` (`every` included), and a `{` or `}` in a pattern, which picomatch
expands and the check does not read.

## What goes wrong when the filter misses a file

A change to a file the filter misses skips the job. That happens on the pull request and again on
main: on a push, `changes` selects by that push's own diff, so main skips the job too. Nothing
re-runs it later, and the break shows up in the next change that does select the job, far from its
cause.

So when a change makes a job's command, or a binary its tests build, compile a new package (an import
in production code or in a test), embed a file, or read a new file, or makes a test start a new go
command, re-run the method and extend the filter in the same pull request. A file read only behind an
environment variable CI never sets stays out.

## check-ci-filters.sh holds every filter to what its job reads

The `ci-filters` job runs `.github/scripts/check-ci-filters.sh` on every change to Go code, a
module or workspace file, the release target lists, `ci.yaml` or the check itself. For each gate
in `ci_job_reads.py` it reruns the method above with `go list` over the job's go commands, adds
the gate's other reads, and fails naming each file the job reads that its filter misses, with
the go command or step that reads it. So an import that pulls a package outside a Go job's filter
fails the pull request that adds it.

`ci_job_reads.py` is the part the check cannot find by itself, kept by hand:

- `GATES`: per filter, the job, its go commands and its other steps' reads. Each go command is a
  `Run` pinned to its text: `step_run` for a `run:` step of the job, `script_run` for a script the
  job runs, pinned to the script's whole line. The check fails when a job's `run:` step starts a
  go command (`go test`, `vet`, `build`, `run`, `list`, `install`, `generate`) nothing claims.
  It reduces a claimed command only in its simple form: one line of `go test|vet|build`,
  relative package paths and only `-v`, `-race`, `-count=N`, `-timeout D`, `-buildvcs=false` and
  one `-tags T`, that is its step's whole `run:` script (split at `\n` alone, stripped of spaces
  and tabs, comments and blank lines aside; any character outside printable ASCII, tab and `\n`
  makes it not simple), with no `shell:` on the step, no `defaults.run.shell` on the job or the
  workflow, no `container:` on the job, no `env:` name outside `ENV_ALLOWED` in the workflow's,
  job's or step's `env:` (test configuration go never reads; `GO*`, `CGO_*`, `BASH_ENV`, `ENV`,
  `HOME`, `XDG_CONFIG_HOME` and `PATH` are outside it, and a new name joins it only when go does
  not read it), no step of the job writing `$GITHUB_ENV` or `$GITHUB_PATH` or running
  `go env -w`, and the step's working directory (its own, else a `defaults.run` one, else the
  root) the `Run`'s; its packages, tags, `-race` and mode must then be exactly the `Run`'s (a
  `-race` line's `Run` says `race=True`, and its closure is what `go list -race` lists), and a
  script's line must be exactly the pinned text. Any other claimed command (`GOFLAGS=...`,
  another flag, a pipe, a quote, a continuation, a `cd`, another line in its step), and one whose
  step runs in another directory, counts as reading every Go file of the repository, every file
  under a Go module's directory (each directory with a `go.mod`, testdata and embedded files
  included) and every `go.mod`, `go.sum`, `go.work` and `go.work.sum`, and fails naming the
  command unless its filter selects them all. Pass a value to a later step as a step output set
  in that step's `env:`, not through `$GITHUB_ENV`. The check fails wide rather than parse
  further.

  The check reads `ci.yaml`'s own steps, `env:` blocks and defaults and the scripts and steps the
  table pins. It does not read the environment a local action or a repository script writes (a
  composite action's own `$GITHUB_ENV`, a script's `export`), and it reads a pinned repository
  script the way Python splits lines, so a `\r` hidden inside one is outside it too. It answers
  accidental drift, a new import that silently stops a job running on it; a command or flag
  hidden on purpose where it does not read is the review's to catch.

  A step whose go commands are in no simple form (a loop, a pipe, `GOOS=... go build`) gets an
  exact entry instead of `step_run`s: add `pinned_step(job, step, workdir, script, runs)` to the
  gate's `exact=[...]`, with `script` the step's whole `run:` text as YAML reads it, `workdir`
  the directory the step runs in, and one `runs` item `(workdir, packages, what it compiles,
  options)` per go command the script runs, naming exactly the packages, tags, mode, `race` and
  `env` (`GOOS`, `GOARCH`) it compiles them with; a loop over targets is one item per target.
  While the step holds exactly that script (after trailing whitespace) in that directory, under
  the env, shell, container and job above, every go line of it is claimed and those runs'
  closures are checked. Any edit of the script makes every one of its runs count as reading
  everything, as above, so change the entry with the step. envoy-go's unit-test and pack steps
  and envoy-smoke's plugin-schema step are exact entries.

  It fails too when a pinned text or an exact entry's step is gone, and when a job or step is
  gated on a filter neither `GATES` nor `EXCLUDED` lists. A job with no go commands still gets a
  gate with its reads.
- `RULES`: per source file, what a test does outside its package: a go command it starts, or a
  file it reads through `..`. Add one in the change that makes a test do it.
- `EXCLUDED`: `worker_image`, whose image inputs `check-image-trigger-paths.sh` proves instead.
  `envoy_smoke` is a gate for its job's go commands and other step reads; its image inputs are
  `check-image-trigger-paths.sh`'s too.

The non-Go reads and `RULES` are what the check is told, not what it finds: it proves the filters
cover them, not that they are complete, and a new file under a glob a filter names only in part
runs the check only when the change also touches one of its triggers. It runs go with
`GOPROXY=off` over an empty module cache, so it downloads nothing and gives the same answer on a
cold runner as on a warm machine; third-party packages fail to load, which loses no first-party
package. Run it locally from anywhere: `.github/scripts/check-ci-filters.sh`.

## Caches: only main writes

No pull-request or merge-queue run writes an Actions cache entry. A pull request's entries are
scoped to its ref, no other pull request reads them, and they count against the repository's cache
limit, where they evict main's entries.

- Give `actions/setup-go` `cache: false` and `id: setup-go`.
- Restore with `actions/cache/restore`, keyed on the Go version setup-go installed
  (`key: <family>-${{ runner.os }}-go${{ steps.setup-go.outputs.go-version }}`), with the
  family's prefix as `restore-keys` only off main:
  `restore-keys: ${{ github.ref != 'refs/heads/main' && format('<family>-{0}-', runner.os) || '' }}`
  (the families are `go-daemon`, `go-docs`, `go-envoy`, `go-envoy-infra`, `go-notices`). Main
  restores by the exact key only, so the entry it saves after a Go bump holds nothing the older Go
  built; a pull request still falls back to the prefix. Each family keeps one entry, refreshed only
  when Go is bumped. Do not
  key on go.sum: every go.sum change would leave a whole entry, up to a gigabyte, for seven days. A
  go.sum change downloads only the changed modules, and Go's build cache is content-addressed, so a
  stale entry costs a run only what changed. Share a family only with the jobs that build the same
  packages, or whichever job saves first owns the entry the others then miss.
- Save with `actions/cache/save` as the job's last step, with
  `if: github.event_name == 'push' && success() && steps.go-cache.outputs.cache-hit != 'true'`.
  The workflow's only push trigger is main.
- The release workflows' Go builds (release.yaml's `legion_build`, release-envoy-infra.yaml's
  `build`) restore `go-daemon` and `go-envoy-infra` and save nothing, so they keep the prefix as
  `restore-keys` on main too: an entry they never save cannot grow.
- `.github/actions/setup-bun` reads and writes its cache only on main; use it as it is.

## The merge queue runs the router too

ci.yaml triggers on `merge_group` (`checks_requested`). On a group, `changes` selects by the
group's `base_sha..head_sha` diff, what the queued change touches against the queue base, and
`ci-ok` reports on the group's commit, so the required checks pass on the merged tree before
anything lands. A new job needs nothing extra for it. But a job whose `if:` names
`github.event_name` must admit `merge_group`, or it skips on the group while its output selects
it, and `ci-ok` fails there. What skips a group does so on purpose: the cache saves (push only),
lint's pull-request text check (pr-title.yaml checks the text on the group), and worker-image
(pull request only, so it is in `PULL_REQUEST_ONLY`; main's image builds are release.yaml's). The
docs job runs on a group and on a pull request but on no push (its output is false there): docs-pages.yaml
builds and publishes main's site.

## Drafts run no test job

`changes` and the jobs it selects skip on a draft pull request, and `ci-ok` fails with "Draft pull
request: no test job runs. Mark it ready for review to start them." It fails rather than passes
because a skipped job reports success: a draft's `ci-ok` would otherwise pass with nothing tested.
Marking the pull request ready for review (`ready_for_review`) starts a run that selects by the
same diff. A job with `needs: changes` and a plain `if:` skips when `changes` skips, so it keeps
the rule. A job whose `if:` starts with `!cancelled()`, to run when `changes` failed as lint,
typecheck and test do, must also carry `!github.event.pull_request.draft`.

## Main pushes are never cancelled

A newer push to a pull request cancels its older run. A push to main is a concurrency group of its
own per commit and is never cancelled: `changes` selects by that push's own diff, and no later
push's diff holds the jobs a dropped run would have selected. A merge group is likewise a group of
its own per commit and is never cancelled. A cancelled main run would leave its commit untested for
good. Keep a new job inside the workflow's group; do not give it a `concurrency:` that cancels on
push.

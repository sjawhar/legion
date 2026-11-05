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
  - "ci-ok never appears on a pull request although every job finished"
  - "ci-ok stays failed after a re-run of failed jobs left a job changes selected skipped"
  - "ci-ok fails saying it does not know what selects a job"
  - "a pull request run writes an Actions cache entry"
---

# Join a Job to the CI Router

`.github/workflows/ci.yaml` (name `CI`) is the one workflow every pull request and every
merge-queue group runs, and the bypass check dispatches. It has no `paths:`. Its first job,
`changes`, reads the paths
the change touched and selects jobs through one dorny/paths-filter filter per job. `ci-ok`
condenses every job's result into one check, and no job is `ci-ok`: every job but `changes` ends
with the `ci-ok-report` step, and the last job of the run to finish posts `ci-ok` from it (the
section on `ci-ok` below). The deployment repository keeps its own router guide for its own
workflows; this page covers this repository only.

## The seven places a job joins

1. **A filter in `changes`** that names every file the job reads (the method is below).
2. **An output of `changes`** for that filter.
   Every output carries the dispatched term, since a dispatched run skips the filter step and selects every job:
   `<filter>: ${{ github.event_name == 'workflow_dispatch' && 'true' || steps.filter.outputs.<filter> }}`.
   A slow suite carries the `full-ci` term too (step 5 says why), and ends with the `'false'` that makes the skip `changes`' own:
   `<filter>: ${{ github.event_name == 'workflow_dispatch' && 'true' || (github.event_name != 'pull_request' || contains(github.event.pull_request.labels.*.name, 'full-ci')) && steps.filter.outputs.<filter> || 'false' }}`.
3. **`needs: changes`** on the job, and
   `if: needs.changes.outputs.<filter> == 'true' && (github.event_name != 'merge_group' || github.run_attempt == '1')`.
   The second term holds the job back on a re-run attempt of a merge-group run, where the group is tested already.
   Every job of `ci.yaml` carries a term of its own for that and `check-ci-filters.sh` fails one that does not:
   `gh run rerun --failed` leaves out a `changes` that succeeded and hands the attempt that job's outputs again, so no job may lean on `changes`' skip for it.
4. **The `ci-ok-report` step as the job's last step**, the same text in every job:

   ```yaml
   - name: Report ci-ok when this is the run's last job
     if: always()
     uses: ./.github/actions/ci-ok-report
     with:
       changes: ${{ toJSON(needs.changes) }}
       job-status: ${{ job.status }}
   ```

   A job that calls a workflow passes it `ci-ok-changes: ${{ toJSON(needs.changes) }}`, and that
   workflow's job ends with the step under `job: <the calling job's id>` (docs-site.yaml and
   worker-image.yaml do). A job with its own `permissions:` block repeats `actions: read` and
   `checks: write`, which the step needs. A run whose last job lacks the step never gets `ci-ok`.
5. **The job in `CI_OK_SELECTED_BY`** (ci.yaml's `env:`), mapped to the output its `if:` tests
   (`"<job>": "<filter>"`), and in `CI_OK_PULL_REQUEST_ONLY` too when its `if:` also admits only
   `github.event_name == 'pull_request'`. A job an event must hold back gets its `changes` output
   set false on that event instead of an `if:` that skips it, so `ci-ok` reads the skip as
   unselected: that is how the nine slow suites are held back on a pull request that does not
   carry the label `full-ci` (`(github.event_name != 'pull_request' ||
   contains(github.event.pull_request.labels.*.name, 'full-ci')) && steps.filter.outputs.<filter>
   || 'false'`). `ci-ok` waits for the report of
   every job the map says `changes` selected and fails one the map does not name, and
   `check-ci-filters.sh` fails when the map or a list is not exactly what the `if:`s test, or a
   job outside `CI_OK_EXEMPT` lacks the report step (the section on `ci-ok` below says why).
6. **A `cancel-<job>` follower**, and that follower's id in `CI_OK_EXEMPT`.
   It is the same job every time: `needs: <job>`, the one condition every follower carries, a sparse checkout of `.github` and `.github/actions/merge-group-cancel` with `needs: ${{ toJSON(needs) }}`.
   The merge-queue section below says what it does.
   A job `ci-ok` waits for that has none leaves every group built on a failed one running to its end, and `check-ci-filters.sh` fails it.
   A job that runs only on a pull request takes no follower, since the queue never runs it.
7. **A gate in `.github/scripts/ci_job_reads.py`** that says what the job reads, which
   `.github/scripts/check-ci-filters.sh` holds the filter to (the section on the check below).

A Go job also restores its caches the way the section on caches below says.

## ci-ok is posted by the run's last job, and fails a selected job that did not succeed

A `ci-ok` job would wait for a runner of its own once every other job ended, behind every job the
account started meanwhile: on 2026-11-02, with the account's runners busy all day, that wait took
a median 39 minutes a run and up to 111. So no job posts it. The `ci-ok-report` step uploads the
job's `job.status` as an artifact of the run (`ci-ok-<job>-<run attempt>-<status>`), then runs
`.github/scripts/ci-ok-report.sh`, which lists the run's artifacts and, once lint and every job
`changes` selected have reported, posts `ci-ok` on the head with the job's own `GITHUB_TOKEN`
(the GitHub Actions App, the source main's ruleset names). Until then it says which jobs it
waits on and exits 0; the last of them to finish posts. A re-run attempt's report replaces that
job's earlier one.

The verdict is `.github/scripts/ci-ok.sh`'s. It passes only when `changes` succeeded, lint
succeeded (it runs on every change), every job `changes` selected succeeded, and every job it
did not select succeeded or was skipped. ci-images is one of the selected jobs: the `ci_images`
filter selects it, and `CI_OK_SELECTED_BY` maps it. A selected job that failed, was cancelled or
timed out fails it, and it prints every job's result and whether `changes` selected it.

A skipped job reports success, so accepting any skip is not enough. When `changes` fails, every
job its filters gate is skipped. GitHub's "Re-run failed jobs" then re-runs `changes`, and
`changes` can succeed while the jobs it now selects stay skipped: they never report, so the run
posts no new `ci-ok` and the failed one stands. So `ci-ok-report.sh` and `ci-ok.sh` decide what
`changes` selected from its outputs, through `CI_OK_SELECTED_BY` (one reading, in
`.github/scripts/ci-ok.jq`): a job is selected when the output it maps to is `'true'`, and for a
job in `CI_OK_PULL_REQUEST_ONLY` the run must also be a pull request's (`worker-image` is the one
such job; main's image builds are release.yaml's). Re-run the whole workflow instead, on a
`pull_request` run: every job of a re-run attempt of a merge-group run skips, so a merge-group
run is never re-run, and the pull request is tested again when it is queued again.

`check-ci-filters.sh` fails when `CI_OK_SELECTED_BY` is not exactly {job: the output its `if:`
tests} for every job whose `if:` tests an output of `changes`, when `CI_OK_PULL_REQUEST_ONLY` is
not exactly the jobs whose `if:` restricts to `github.event_name == 'pull_request'`, when a job
but `changes` and those `CI_OK_EXEMPT` names does not end with the report step, when a job
`CI_OK_EXEMPT` names does carry it, and when a job is named `ci-ok` or `pr-title`,
which would post a required check itself (a skipped job posts a pass). A wrong entry would fail a
job its `if:` rightly skipped, pass one a re-run left skipped, or post `ci-ok` before a job
reported.

A fork pull request's run gets a read-only token, which can post no check run, so a fork pull
request gets no `ci-ok` and no push-time `pr-title`: this repository takes no fork pull
requests, whose runs wait for an approval nobody gives.

A job whose main half must run on a push to main, as the docs site's Pages deploy must, cannot be
a job of the router: the router runs on no push. Its main half is a push-only workflow of its own
with a workflow-level group keyed on the ref (`cancel-in-progress: false`, and no `queue:` key,
so of the pushes waiting only the newest builds), whose `on.push.paths` equals the job's filter,
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

- the composite actions it uses (`.github/actions/<name>/**`) and what they read, but for the
  `ci-ok-report` step every job ends with: lint runs it at every change, and the test job runs
  `ci-ok-report.test.sh` when its scripts change, so a job's filter does not name it;
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

A change to a file the filter misses skips the job. That happens on the pull request and again in
the merge group that lands it: `changes` selects there by what the queued change touches against
the queue base, so the group skips the job too, and no run on main follows. Nothing
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

## Caches: no job of the router writes one

No run of `ci.yaml` saves an Actions cache entry. A pull request's entries are scoped to its ref,
no other pull request reads them, and they count against the repository's cache limit, where they
evict main's entries; a merge group's entries are no more readable than those. Only an event that
writes the default branch's scope — a push to main — leaves an entry every later run can read, so
the fills live in workflows that run there.

- Give `actions/setup-go` `cache: false` and `id: setup-go`.
- Restore with `actions/cache/restore`, keyed on the Go version setup-go installed
  (`key: <family>-${{ runner.os }}-go${{ steps.setup-go.outputs.go-version }}`), with the
  family's prefix as `restore-keys`
  (`restore-keys: ${{ format('<family>-{0}-', runner.os) }}`; the families are `go-daemon`,
  `go-docs`, `go-envoy`, `go-envoy-test`, `go-envoy-infra`, `go-notices`). In `ci.yaml` that
  fallback applies on every run, a dispatched one on main included, because no job there saves.
  A job that does save keeps the prefix off main alone
  (`restore-keys: ${{ github.ref != 'refs/heads/main' && format('<family>-{0}-', runner.os) || '' }}`),
  so the entry it saves after a Go bump holds nothing the older Go built; docs-site.yaml's build,
  which docs-pages.yaml calls on main, is the one such job. Each family keeps one
  entry, refreshed only when Go is bumped. Do not
  key on go.sum: every go.sum change would leave a whole entry, up to a gigabyte, for seven days. A
  go.sum change downloads only the changed modules, and Go's build cache is content-addressed, so a
  stale entry costs a run only what changed. Share a family only with the jobs that build the same
  packages, or whichever job saves first owns the entry the others then miss.
- Add no `actions/cache/save` step here, and no `if:` that tests `github.event_name == 'push'` or
  compares `github.ref` with a branch ref: the workflow runs on no push, so such a condition gates
  its step on what no run ever is, and `check-ci-filters.sh` refuses it. A family a new job
  restores is filled by `.github/workflows/ci-caches.yaml`, which runs on a push to main, looks
  each key up (`lookup-only: true`, no `restore-keys`) and, only on a miss, runs what the job
  builds and saves it under exactly the key and paths the restore names; `check-ci-filters.sh`
  holds the save to the restore and fails when the two drift. `go-docs` is the exception:
  docs-pages.yaml saves it on that same push, from the build job it shares with ci.yaml's docs
  job.
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
it, and the run posts no `ci-ok` there, since `ci-ok` waits for that job's report. What skips a
group does so on purpose: lint's pull-request text check
(pr-title.yaml checks the text on the group), and worker-image (pull request only, so it is in
`CI_OK_PULL_REQUEST_ONLY`; main's image builds are release.yaml's). The nine slow suites go the
other way: the queue is where their result normally comes from, and a pull request runs them only
when it carries the label `full-ci`. The
docs job runs on a group and on a pull request alike; docs-pages.yaml builds and publishes main's
site on a push trigger of its own.

Two things a group's run alone carries.
Every job of a re-run attempt skips, so a merge-group run is never re-run: a group is tested once, and the pull request is tested again when it is queued again.
Each job carries the term that does it, which step 3 above gives.
And the run of a group that can no longer merge is force-cancelled by jobs of the workflow itself — `cancel-started` and one follower per job `ci-ok` waits for, plus one for `changes` — which run only on `merge_group`, report nothing and are therefore named in `CI_OK_EXEMPT` rather than in `CI_OK_SELECTED_BY`.
A follower reads the report its job uploaded before it ended, so it acts only when that job's own work failed: a job whose tests passed and whose `ci-ok-report` step alone failed cancels nothing, and a job that uploaded none leaves this run alive to post the group's `ci-ok`.
Step 6 above is the follower a new job takes with it.

## Drafts run no test job

`changes` and the jobs it selects skip on a draft pull request, so no job reports and the draft
gets no `ci-ok`, which keeps it from merging with nothing tested (a skipped job reports success,
so a passing check there would be a lie). Lint posts no `pr-title` there either; pr-title.yaml
still checks a draft's text when it is opened, reopened or edited. Marking the pull request
ready for review (`ready_for_review`) starts a run that selects by the same diff. A job with
`needs: changes` and a plain `if:` skips when `changes` skips, so it keeps the rule. A job whose
`if:` starts with `!cancelled()`, to run when `changes` failed as lint, typecheck and test do,
must also carry `!github.event.pull_request.draft`.

## A merge group's run is never cancelled by a newer one

A newer push to a pull request cancels its older run. A merge group is a concurrency group of its
own per commit and is never cancelled that way: the queue waits for its `ci-ok`, and a cancelled
run would leave the tree it tests unjudged. A dispatched run keys on the commit it runs at and is
never cancelled either. Only two acts stop such a run, both deliberate: the cancel jobs above,
when the group can no longer merge, and `.github/workflows/dequeued.yaml`, when the group's pull
request leaves the queue. Keep a new job inside the workflow's group; do not give it a
`concurrency:` of its own.

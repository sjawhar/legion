---
title: "Join a Job to the CI Router: A Filter That Names Every File the Job Reads"
category: github
tags:
  - github-actions
  - path-filters
  - ci-router
  - caching
  - concurrency
date: 2026-10-27
status: active
module: ci
related_issues:
  - "INFRA-520"
symptoms:
  - "a change merged without a job that reads one of its files, and nothing re-ran the job"
  - "a new job never runs on a pull request, or runs on every pull request"
  - "ci-ok fails on a draft pull request although no job failed"
  - "a pull request run writes an Actions cache entry"
---

# Join a Job to the CI Router

`.github/workflows/ci.yaml` (name `CI`) is the one workflow every pull request and every push to
main runs. It has no `paths:`. Its first job, `changes`, reads the paths the change touched and
selects jobs through one dorny/paths-filter filter per job. Its last job, `ci-ok`, condenses every
job's result into one check. The deployment repository keeps its own router guide for its own
workflows; this page covers this repository only.

## The four places a job joins

1. **A filter in `changes`** that names every file the job reads (the method is below).
2. **An output of `changes`** for that filter: `<filter>: ${{ steps.filter.outputs.<filter> }}`.
3. **`needs: changes`** on the job, and `if: needs.changes.outputs.<filter> == 'true'`.
4. **The job in `ci-ok`'s `needs:`.** A job that is not there can fail and still let `ci-ok`
   pass.

A Go job also restores its caches the way the section on caches below says.

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

Write no `!` pattern. The step's quantifier is `some`: a change matches a filter when any pattern
matches, so a negated pattern matches every file outside it and selects the job on almost every
change. `.github/scripts/check-image-trigger-paths.sh` also reads the image gates (`worker_image`,
`envoy_smoke`) only as flat lists of globs, so those two name no anchor and no negation.

## What goes wrong when the filter misses a file

A change to a file the filter misses skips the job. That happens on the pull request and again on
main: on a push, `changes` selects by that push's own diff, so main skips the job too. Nothing
re-runs it later, and the break shows up in the next change that does select the job, far from its
cause.

So when you add an import, an embed, a go command a test starts, or a test that reads a new file,
re-run the method and extend the filter in the same pull request.

## Caches: only main writes

No pull-request run writes an Actions cache entry. A pull request's entries are scoped to its ref,
no other pull request reads them, and they count against the repository's cache limit, where they
evict main's entries.

- Give `actions/setup-go` `cache: false`.
- Restore with `actions/cache/restore`, keyed on the family's sums, with the family's prefix as
  `restore-keys` (`go-daemon`, `go-envoy`, `go-envoy-infra`, `go-notices`). Share a family with the
  jobs that compile the same modules.
- Save with `actions/cache/save` as the job's last step, with
  `if: github.event_name == 'push' && success() && steps.go-cache.outputs.cache-hit != 'true'`.
  The workflow's only push trigger is main.
- `.github/actions/setup-bun` reads and writes its cache only on main; use it as it is.

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
push's diff holds the jobs a dropped run would have selected. A cancelled main run would leave its
commit untested for good. Keep a new job inside the workflow's group; do not give it a
`concurrency:` that cancels on push.

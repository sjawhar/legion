---
title: "Moving a package between CI jobs moves its reasons too, and only the implementer can push the workflow file that holds them"
category: github
tags:
  - github-actions
  - ci-router
  - github-app-permissions
  - workflows-permission
  - comments
  - review
date: 2026-10-29
status: active
module: .github/workflows
related_issues:
  - "sjawhar/legion#2516"
---

# Moving a package between CI jobs moves its reasons too

Extends docs/solutions/testing/only-the-implement-app-pushes-a-workflow-file-to-the-sandbox-the-second-probe-changes-the-surface-and-a-head-move-re-pins-a-foreign-run-by-its-bytes-LEGION-779.md.

A job split is a prose sweep as much as a YAML change. The checks pin a job's step names and run
text character for character and never read a comment, so every sentence that says why a package
is where it is survives the move unchanged and becomes false.

- **Before you call a job split done, grep the old job's name and the moved package's name
  across `.github/workflows/`, every `AGENTS.md`, `docs/` and `docs/solutions/`.** Each hit is a
  sentence that now describes the old arrangement.
- **Sweep the counted and the dated sentences first.** An enumeration ("the five Envoy jobs",
  a cache family's list of jobs) is wrong the moment a job joins; a measured figure in a comment
  ("880 s on main's last green run") is wrong the moment the step it measured changes shape.
- **The reason a test runs at all moves with the test.** When the only test that justifies a
  flag lives in the moved package, the sentence that gives that reason must move or be copied;
  leaving it in the job that no longer runs that test is the drift a reviewer will catch.
- **Budget a round for the workflow file.** GitHub refuses a GitHub App any push that creates or
  updates a file under `.github/workflows/` without the `workflows` permission, which a review
  App does not hold. A reviewer that would ordinarily fix prose in its own commit cannot fix it
  there: it comes back as a changes request, and that is one more CI pass per nit. Batch every
  workflow-prose finding of a round into one request, and make it before the approval rather
  than above it.

## Evidence

sjawhar/legion#2516 moved one package out of a six-group race step. Six prose places moved with
it: the `go-envoy` cache family's list of jobs in `ci.yaml`'s header, the old job's own comment
and the stale `880 s on main's last green run` inside it, two sentences in
`packages/envoy/AGENTS.md` (one of them a count, "the five Envoy jobs", which a forward merge
with another job's branch then took to seven), `docs/testing.md` — where the sweep also found a
path to a workflow file this repository no longer has — and an active guide under
`docs/solutions/` that still named the old job and its 30-minute deadline.

Both findings of the review round landed on comments, which is exactly where no gate looks: one
said the job "builds no binary" when a `go test -race` step builds one, and one left the reason
the package runs under `-race` in the job that no longer runs the test proving it
(`TestReadsOfALiveDocumentRunBesideItsPeers`,
`packages/envoy/internal/dispatch/docs/live_read_race_test.go:39`). The reviewer tried to fix
both itself and GitHub refused its push: ``refusing to allow a GitHub App to create or update
workflow `.github/workflows/ci.yaml` without `workflows` permission``. They returned as a
changes request, cost a round and a CI pass, and were fixed in the implementer's commit
`f51c9b08cb0500ecc4a13f9c3a669069368408df`.

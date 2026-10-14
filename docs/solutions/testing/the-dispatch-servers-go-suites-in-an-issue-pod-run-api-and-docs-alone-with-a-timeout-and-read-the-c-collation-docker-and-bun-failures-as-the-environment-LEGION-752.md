---
title: "The Dispatch server's Go suites in an issue pod: run api and docs alone with a timeout, and read the C-collation, Docker and bun failures as the environment"
category: testing
tags:
  - worker-pod
  - dispatch-server
  - go-test
  - postgres
  - collation
  - proof-environment
date: 2026-10-14
status: active
module: packages/envoy/internal/dispatch
related_issues:
  - "LEGION-752"
  - "sjawhar/legion#1957"
---

# The Dispatch server's Go suites in an issue pod: run api and docs alone with a timeout, and read the C-collation, Docker and bun failures as the environment

Extends docs/solutions/testing/go-test-in-a-worker-pod-leaves-gowork-to-go-work-and-names-the-packages-the-pod-cannot-run-LEGION-630.md.

- A Postgres built from Debian packages under `/tmp` in an issue pod
  (`docs/solutions/legion/a-port-held-in-an-issue-pod-is-a-sibling-roles-harness-move-your-run-with-the-five-port-variables-and-your-own-database-and-never-stop-the-holder-LEGION-695.md`)
  runs the Dispatch server's database suites, but `initdb` there has only `C`, `C.utf8` and `POSIX`
  (`locale -a`), so its default collation is byte order and
  `TestIssueRanksStayOrderedPastLowercaseDigitsAndProductionShapedMoves` (`internal/dispatch/api`)
  refuses it by its own message (`this test's database already orders "a" after "B" … run it
  against a database whose default collation is not already byte order`). Read that as the
  environment, name it in the handoff, and point at CI's `envoy-go` job, whose Postgres has
  `en_US.utf8`.
- Do not run `go test ./internal/dispatch/... -count=1` as one command in the pod to prove a
  change: `api` and `docs` then share the pod's CPU and the one Postgres with every other package
  and overrun `go test`'s ten-minute per-package default (`panic: test timed out after 10m0s`,
  `FAIL … 600.0s`), which reads as a failure of the branch. Run the touched packages alone with a
  budget - `go test ./internal/dispatch/docs/ -count=1 -timeout 40m` (about 400 s) and
  `go test ./internal/dispatch/api/ -count=1 -timeout 55m` (about 700 s) - and the selection the
  plan names first, since the whole package is a wall-clock cost the pod repays slowly.
- Expect these to fail in the pod for the environment: `internal/dispatch/delivery`,
  `internal/dispatch/outbox`, `internal/dispatch/redeliver` and `internal/dispatch/files`
  (testcontainers for NATS and an S3-compatible server: `panic: rootless Docker not found`);
  `internal/dispatch/pmdoc`'s browser-parity tests (`TestRenderReadsBackInBothParsersAsTheEditorShowsIt`,
  `TestUpdate…`, `TestNullAttributesSurviveABrowserEdit`: `Cannot find module '@legion/proof-editor'`
  until `bun install` has run in the workspace) and its two linearity tests
  (`TestParseAndRenderAreLinearOnDelimiterAndOpenerHeavyText`, `TestUpdateIsLinearInTheBlockCount`,
  timing under gVisor). None is touched by a change inside `docs` or `api`.
- The `-timeout` is for the pod. CI's `envoy-go` job runs the whole tree on a runner with Docker
  and a Postgres with the right collation; cite its run in the PR body's `CI` line for everything
  the pod cannot run.

## Evidence (LEGION-752, sjawhar/legion#1957)

The change was one file in `internal/dispatch/docs` and its tests. The plan's last command,
`go test ./internal/dispatch/... -count=1`, run once in the pod against a Postgres 17.11 built
from trixie packages on port 55434: `api` and `docs` timed out at 600 s, `delivery`, `outbox`,
`redeliver` and `files` panicked on the missing Docker, `pmdoc` failed eight tests on the missing
`@legion/proof-editor` module and two linearity budgets, and the rest passed. Run alone,
`docs` passed in 400 s and `api` in 705 s with the one collation refusal; the tester, on its own
Postgres of the same build, met the same refusal and read it the same way. CI's `Tests` run
38610540867 and `Legion Envoy and Contracts` run 38610540851 at `104d13b8` were green.

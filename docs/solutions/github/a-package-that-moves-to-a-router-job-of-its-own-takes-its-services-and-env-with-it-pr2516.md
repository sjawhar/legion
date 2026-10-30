---
title: "A package that moves to a router job of its own takes its services and env with it, or the job is green having run almost nothing"
category: github
tags:
  - github-actions
  - ci-router
  - postgres
  - services
  - go-test
  - skip
date: 2026-10-29
status: active
module: .github/workflows
related_issues:
  - "sjawhar/legion#2516"
---

# A package that moves to a router job of its own takes its services and env with it

Extends docs/solutions/testing/the-envoy-brokers-postgres-tests-skip-without-broker-test-database-url-so-a-proof-counts-zero-skips-INFRA-379.md.

`docs/solutions/github/join-a-job-to-the-ci-router.md` lists six places a job joins the router.
A job that takes a package away from another job has a seventh: the `services:` and `env:` that
package's tests need. Nothing in the router checks it, because a Go package whose tests skip
still reports `ok`, a skipped test is a pass, and a passing job satisfies both gates — the
drift check decides file coverage and `ci-ok` decides success.

- **Before you write the new job, name the variable the moved package's `TestMain` skips on, and
  say who provides it there.** Read the package's `TestMain`, then the helper it calls: a
  `t.Skip` on an unset variable is the whole trap.
- **Copy the `services:` block and the `env:` line from the job the package is leaving**, and
  write in the file that they are load-bearing, with the file and line of the skip. A later
  reader deletes an unexplained service.
- **Prove the service is load-bearing with the skip as the negative control**, before the job
  ever runs: run the package's own command with the variable unset and show `--- SKIP` together
  with the package's `ok`. The control is valid only if a `--- SKIP` line appears — `go test
  -run` matches an unanchored regexp, so a name no test carries prints
  `testing: warning: no tests to run` and `ok`, which proves nothing.
- **Take away what only the moved package needed.** The same move runs the other way: a setup
  step the job kept for that package alone (a toolchain install, a workspace link) belongs in
  the new job and nowhere else.

## Evidence

In sjawhar/legion, `packages/envoy/internal/dispatch/docs` has
`func TestMain(m *testing.M) { os.Exit(storetest.Main(m)) }`
(`internal/dispatch/docs/persistence_test.go:19`), and `storetest.Open` calls
`t.Skip(DatabaseURLEnv + " must be set to run Postgres-backed Dispatch tests")` when the
variable is empty (`internal/dispatch/store/storetest/storetest.go:57-61`). The job the package
left carried the Postgres service and `DISPATCH_TEST_DATABASE_URL`; a new job without them
would have been green having run almost nothing, and both gates would have accepted it
(`envoy-go-race-docs: 755 files, 0 uncovered`, `ci-ok` success).

sjawhar/legion#2516 copied the service and the variable and recorded the control: with
`DISPATCH_TEST_DATABASE_URL` unset,
`CGO_ENABLED=1 go test -race -v -run '^TestChangesRequestedSinceFollowsTheOrderTheReviewsCommitted$' ./internal/dispatch/docs/`
printed `--- SKIP` with that message and the package still reported `ok`. On the job's first
real run (run 40921821162, job 123625657214) the same package reported
`ok  github.com/sjawhar/envoy/internal/dispatch/docs  1015.560s`, and the log held no
`must be set` line.

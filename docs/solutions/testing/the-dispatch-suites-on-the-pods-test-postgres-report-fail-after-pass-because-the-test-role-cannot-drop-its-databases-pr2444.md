---
title: "The Dispatch suites on the pod's test Postgres report FAIL after PASS, because the test role cannot drop its databases"
category: testing
tags:
  - issue-pod
  - postgres
  - storetest
  - dispatch
date: 2026-11-02
status: active
module: packages/envoy/internal/dispatch/store/storetest
related_issues:
  - "sjawhar/legion#2444"
---

# The Dispatch suites on the pod's test Postgres report FAIL after PASS, because the test role cannot drop its databases

Extends docs/solutions/legion/the-pod-serves-its-own-test-postgres-now-and-legion-does-not-survive-a-pod-replacement-pr2549.md.

- On the pod's own test Postgres (`LEGION_TEST_PG_DSN`, role `legion_test`), the role can create
  databases but is not a superuser. `storetest` cannot drop its isolated databases there and
  prints `drop isolated database …: ERROR: permission denied to terminate process (SQLSTATE
  42501)`, so `go test` marks the package `FAIL` after every test has passed.
- Read the package's `PASS` line and its `--- FAIL` lines before you count a package red. A
  package whose only failure is that teardown line passed.
- Expect three store census tests to fail there, since each needs a superuser: an event trigger,
  a grant of `pg_read_all_stats`, and a replication connection. Run the store suite against a
  database the role owns (`createdb -h 127.0.0.1 -p 5433 -U legion_test <name>`). Its migration
  tests then pass, but the census tests do not.
- Name these failures as the environment's in the handoff, and say that CI ran them green.

## Evidence

- sjawhar/legion#2444's round 17 ran after a pod replacement on Postgres 18.6. `docs`, `api`,
  `cmd/dispatch` and `outbox` each printed `PASS` followed by the teardown error and `FAIL`.
  `store` failed `TestCensusRefusesALockHolderWhoseAgeItCannotSee`,
  `TestCensusRefusesAWALSenderHoldingALockInATransaction` and
  `TestCensusDoesNotExemptACatalogMigrationAnEventTriggerRunsOn` with permission errors. CI ran
  the same code green at f3c318da (run 41405799632).

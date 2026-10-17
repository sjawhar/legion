---
title: "A renumbered daemon migration leaves the pod's shared embedded Postgres skipping main's file at the old number and refusing its own: recreate the public schema before the next suite run"
category: testing
tags:
  - migrations
  - schema_version
  - renumber
  - forward-merge
  - embedded-postgres
  - worker-pod
  - LEGION_TEST_PG_DSN
date: 2026-10-17
status: active
module: packages/daemon/internal/store/migrations
related_issues:
  - "LEGION-694"
  - "sjawhar/legion#1999"
---

# A renumbered daemon migration leaves the pod's shared embedded Postgres skipping main's file at the old number and refusing its own: recreate the public schema before the next suite run

Extends docs/solutions/envoy/two-pull-requests-take-the-same-migration-number.md.

- The daemon's ledger is one row per version in `schema_version` (`internal/store/store.go`
  `Migrate`/`apply`), keyed by the number alone. A database that applied your file at its old
  number has that number recorded and your DDL in place; after the renumber it skips `main`'s file
  that now carries the old number as already applied (silently: `main`'s schema never lands), and
  refuses yours at the new number (`relation "…" already exists`). Neither is a flake: every boot
  against that database fails the same way until the schema is recreated.
- Which suites feel it is a property of their database, not of the renumber. Every suite that
  takes a database of its own per test (`testpg.Database`, `internal/store`'s `emptyStore`,
  `internal/record`'s, `internal/api`'s, `internal/intake`'s, `internal/admit`'s and
  `internal/workflow`'s `create database` helpers, `isolatedOutboxPool`) is green, since each
  starts empty; the daemon-process suite in `internal/daemon` (`daemon_test.go` `testDSN`)
  migrates the `LEGION_TEST_PG_DSN` database itself, and it alone fails. A green run of every
  other package after a renumber proves nothing about the shared database.
- In a worker pod the embedded Postgres (docs/solutions/testing/an-embedded-postgres-in-a-worker-pod-the-zonky-jar-unpacked-with-python-gives-the-daemons-database-suites-a-run-LEGION-631.md)
  lives for the pod and has no `psql`; recreate the schema through the pgx runner you boot it
  with: `drop schema public cascade` then `create schema public` on the DSN's database, before the
  first suite run after the renumber. CI never sees the case (a fresh service database per job),
  so the pod run is the only place it shows.

## Evidence

- LEGION-694's fourth forward merge of `main` brought `0043_ready_merge`, `0044_ready_merge_dedup`
  and `0045_ready_merge_backfill`; the branch's `0043_outbox_issue` moved to `0046`
  (`migrations.All` refuses two files sharing a version). The pod's shared database on port 54331
  had recorded 43 from the branch's old file. The first `go test ./internal/daemon/` on the merged
  tree failed every test that boots a daemon, each at the same line:
  `apply migration 0046_outbox_issue.up.sql: ERROR: relation "outbox_issue" already exists (SQLSTATE 42P07)`;
  `internal/record` and `internal/store` were green on the same tree. `drop schema public cascade;
  create schema public` through `/tmp/pgexec/pgexec`, then the same command: green (160 s).
- Only a proof pod's database ever applied the branch's 0043: production's ledger meets 0046
  first, so the renumber is a test-environment incident, never a migration to repair.

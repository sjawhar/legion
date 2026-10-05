---
title: "Two pull requests take the same Dispatch migration number: the second one renumbers to the next free number on main, and every test database that applied its old number is recreated"
category: envoy
tags:
  - dispatch
  - migrations
  - pgmigrate
  - rebase
  - test-database
date: 2026-10-05
status: active
module: packages/envoy/internal/dispatch/store/migrations
applies_when:
  - A rebase onto main brings in a migration whose number your branch already uses
  - The Go tree fails wholesale at "prepare migrated template database" naming two files that share a version
  - After renumbering, a handful of store tests still fail with "column ... already exists" on `migrate`
  - You are about to pick a migration number on a long-lived branch
---

# Two Pull Requests Take the Same Migration Number

## Symptom

LEGION-541's branch added `0069_agent_artifacts.up.sql`. While it was in review, LEGION-542
(sjawhar/legion#1770) merged `0069_issue_task_progress.up.sql` to main and production applied it
(`/healthz` `schema_version: 69`). After the rebase, `go test ./cmd/dispatch/ ./internal/dispatch/...`
reported 1,046 failures. Every one had the same first line:

```
prepare migrated template database: migrate template database: migrations refused, none applied:
migrations 0069_agent_artifacts.up.sql and 0069_issue_task_progress.up.sql share version 69:
a runner records a migration by its version, so it would apply the first and skip the rest as
already applied; keep the number on the file that merged to main first, since a database may
already have applied it, and renumber the rest to the next free numbers on main
```

That line is `pgmigrate.Load`'s own refusal (`packages/envoy/internal/pgmigrate/set.go`); the
runner refuses the whole set before applying anything, which is why nothing half-migrated.

## Cause

The runner records a migration by its version number alone. Two files with one number would
apply whichever sorts first and skip the other as already applied, so the loader refuses the set.
Main's file keeps the number: a deployed database has already recorded `69` for it, and a branch
that took `69` for something else would have its migration skipped in production forever.

## Fix

1. Rename the branch's files to the next free number on main (`git mv 0069_x.up.sql 0070_x.up.sql`,
   and its `.census.sql`).
2. Renumber every reference to it: the migration's own header comment, the census file's comment,
   the migration test (`TestMigrate0069...` → `0070`, and `migrateThrough(t, store, 68)` → `69`,
   since the seed now runs after main's 0069), code comments that cite the number
   (`api/artifacts.go`'s `artifactOwnerKey`), `cmd/dispatch/AGENTS.md` and `README.md`. A
   `grep -rn 0069 packages/envoy/cmd/dispatch packages/envoy/internal/dispatch` must then name
   only main's migration.
3. Recreate every local database the old number was applied to. Two kinds hide here:
   - The per-package template databases `storetest` clones (`dispatch_template_<hash>`), which a
     killed run can leave behind; they refuse connections (`allow_connections false`), so drop
     them with `update pg_database set datistemplate = false where datname = '...'` first.
   - The shared database `DISPATCH_TEST_DATABASE_URL` names, which `openTestStore` migrates in
     place rather than cloning. It had recorded `69` for the *old* file, so after the renumber the
     runner treated main's 0069 as applied and ran 0070 against a table that already had the
     column: seven `store` tests failed with
     `migration 0070_agent_artifacts.up.sql: ERROR: column "session_id" of relation "artifacts"
     already exists (SQLSTATE 42701)` on freshly cloned databases, because the clones came from
     that database. `drop database dispatch; create database dispatch` cleared it.

## Prevention

- Before opening a PR that carries a migration, `git fetch` and list main's
  `store/migrations/`; take the number after main's highest, not after your branch's.
- On a branch that lives more than a day, re-check at every rebase: the runner's refusal is loud,
  but it costs a full suite run to see, and the stale-database symptom after the renumber looks
  like a different bug.
- The refusal message names the rule; read it before touching anything else, since it tells you
  which file keeps its number.

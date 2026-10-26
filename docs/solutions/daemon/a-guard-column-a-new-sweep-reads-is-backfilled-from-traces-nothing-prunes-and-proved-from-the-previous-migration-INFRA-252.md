---
title: "A guard column a new sweep reads is backfilled from traces nothing prunes, and proved from the previous migration"
category: daemon
tags:
  - migrations
  - backfill
  - upgrade
  - admission
date: 2026-10-26
status: active
module: packages/daemon/internal/store/migrations
related_issues:
  - "INFRA-252"
  - "sjawhar/legion#2226"
---

# A guard column a new sweep reads is backfilled from traces nothing prunes, and proved from the previous migration

Extends docs/solutions/daemon/an-exemption-stored-on-a-record-belongs-to-the-run-the-issue-arrived-with-INFRA-225.md.

- When a change adds a column whose default means "allowed" and a new code path acts on it (a
  sweep, a promotion), ask what the first pass after the migration does to rows an older daemon
  wrote. If it would act on rows the older daemon never meant it for, the migration backfills.
- Backfill only from traces nothing deletes. In the daemon's store: `processed_events` is never
  pruned (an outbox fact's id names its kind and issue, as `handover:<KEY>:<row id>` under source
  `outbox`; match the key with `split_part`, never `LIKE`, since keys prefix each other), and a
  claim keeps the tree it last ran under (0053). The `handovers` fence is deleted at re-admission
  and `handed_over_roots` is written for one case only, so neither answers "did this ever happen".
- Prove it with a migration test that seeds a database at the previous migration number
  (`migrateThrough(t, store, <previous>)`), with rows that must be marked and decoys that must not,
  and run it once with the `update` removed as the negative control.
- Rows no trace tells apart go in the pull request's deploy note as a read-only query an operator
  runs before the new daemon boots, with what to do for each row it lists.

## Evidence

sjawhar/legion#2226 added `issues.handed_over_from` and an adoption sweep that refuses a root
carrying it. Review round 7 found that existing roots an older daemon had handed over carried `''`
and would be adopted back by the first sweep after the deploy. `0057_issue_ancestry.up.sql` now
backfills from `processed_events` and `claims`, `store/migration_0057_test.go` proves it from
version 56 (it fails for the two marked rows with the `update` removed), and the pull request's
deploy note carries the query for an orphan a person admitted that never ran a worker.

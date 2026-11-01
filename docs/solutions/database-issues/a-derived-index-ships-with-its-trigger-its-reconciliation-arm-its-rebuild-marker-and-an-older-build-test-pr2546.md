---
title: "A derived index ships with its trigger, its reconciliation arm, its rebuild marker and an older-build test"
category: database-issues
tags:
  - postgres
  - trigger
  - derived-index
  - rolling-deploy
  - partial-index
date: 2026-10-31
status: active
module: packages/envoy/internal/dispatch/store/migrations
related_issues:
  - "sjawhar/legion#2546"
---

# A derived index ships with its trigger, its reconciliation arm, its rebuild marker and an older-build test

A table whose rows are derived from another table's columns is correct only while every writer of
those columns is a build that knows about it. During a roll, and after a rollback, it is not.
Ship four things in the same change:

1. **A trigger that marks the row owed on every write of each source column**, by insert and by
   update of those columns named one by one — never a blanket update trigger, which owes work on
   every unrelated write. A build that does know about the index clears the mark inside the same
   transaction as the write, so the mark costs nothing on the current build.
2. **An arm in the reconciliation batch and in its owed count**, so a marked row is drained by
   the loop that already runs and the count a tick reads tells you work is waiting.
3. **A marker in the rebuild command**, which owes the row for every existing record, so an
   operator can re-derive the whole table after a parser or grammar change.
4. **A test that an older build's write leaves the row owed.** Migrate to the version before the
   index, write a record, migrate the rest, and assert the row is owed; then, on the current
   schema, assert that a write of each source column separately owes it, that a write of an
   unrelated column owes nothing, that a second write moves only the owed timestamp and keeps
   what has already been derived, and that the reconciliation's owed read uses the partial index.

The migration that seeds the existing rows takes the source table's lock before it seeds, in the
same transaction as the trigger, so no record is written between the two.

## Evidence

sjawhar/legion#2546's derived table is an ask's stored blocks.

- The trigger is
  `packages/envoy/internal/dispatch/store/migrations/20261101143212_asks_block_parts_trigger.up.sql`:
  `create trigger asks_item_blocks_owed after insert or update of question, options, answer on
  asks` (`:36`), with the shared `item_blocks_owe()` function (`:18-28`) and the seed of every
  stored row below it (`:39`). Its header states the lock and seed ordering at `:14-16`.
- The reconciliation arm and the owed count are in
  `packages/envoy/internal/dispatch/docs/block_index.go` (`:236` counts
  `ask_block_state where owed_at is not null`, beside the two existing item kinds).
- The rebuild marker is in `packages/envoy/internal/dispatch/refs/rebuild.go:118-120`, which
  inserts an owed row for every stored record on conflict.
- The test is `TestAnOlderBuildsWriteLeavesTheAskBlocksOwed`
  (`packages/envoy/internal/dispatch/store/ask_blocks_migrations_test.go:60`), whose comment at
  `:54-59` states each of the four assertions above, with
  `TestTheAskBlockMigrationsOweEveryStoredAsk` (`:155`) for the seed. It carries the two version
  constants the migration numbering pins (`:16-19`).

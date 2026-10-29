---
title: "A read reaches an expression index only through its exact expression, and keeps the old predicate beside it"
category: database-issues
tags:
  - postgres
  - expression-index
  - explain
  - generic-plan
  - dispatch
date: 2026-10-28
status: active
module: packages/envoy/internal/dispatch
related_issues:
  - "sjawhar/legion#2467"
---

# A read reaches an expression index only through its exact expression, and keeps the old predicate beside it

- Postgres uses an expression index only for a predicate that compares the index's exact
  expression. `anchor->>'artifact_id' = $1` (text) does not use an index on
  `(anchor->>'artifact_id')::uuid`, and neither uses an index on
  `coalesce(block_artifact_id, (anchor->>'artifact_id')::uuid)`. Copy the expression from the
  migration, character for character.
- Add the indexed expression with `and`, and keep the old predicate. The rows can then only shrink.
  A uuid comparison alone matches more than the text comparison did: two spellings of one uuid
  that differ in letter case.
- Name the rows where the indexed expression and the old predicate disagree (for a `coalesce`, a
  row whose first argument is set to another value), and show that no writer creates one: list
  every insert and update of both columns, and query the data for such rows.
- A cast in the new predicate cannot fail on a stored row when the index already evaluates the
  same cast on every row it holds: a row whose value does not cast could not have been written.
  Check that the parameter casts too, from where the caller got it.
- Prove it on a database with real volume, two ways: `EXPLAIN (ANALYZE, BUFFERS)` of a statement
  prepared with a text parameter under `plan_cache_mode = force_generic_plan`, since the server
  sends `$1` as a bind parameter; and an old-predicate-versus-new-predicate `except` in both
  directions over every key, which must return no rows.

## Evidence

- `packages/envoy/internal/dispatch/docs/marks.go:751-764` (`recordedMarkRefs`, once per
  settlement) compared text and scanned `comments`: Seq Scan, 14,869 rows removed by the filter,
  11.77 ms, on the replay database's 14,887 comments.
- The indexes are `asks_anchor_artifact` and `comments_anchor_artifact`
  (`store/migrations/0032_refs_provenance.up.sql:44,47`). Commit `92a8c7bb` added each table's index
  expression beside the text comparison: Bitmap Index Scan, 0.16 ms, the same 18 rows. Old and new
  predicates selected the same rows for all 1,157 documents (3,400 = 3,400, symmetric difference 0).
- The only writer of `block_artifact_id` is the ask-block insert (`docs/ask_blocks.go:718`), which
  writes no anchor, so an anchored ask's `coalesce` is its anchor's artifact id. No constraint
  enforces this; the comment above `recordedMarkRefs` records the assumption.
- The tester added an anchor with an upper-case artifact id to the equivalence harness. The kept
  text comparison excludes it on both sides.

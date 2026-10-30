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
- One guarded expression, one definition - but a read and an index can need two of them. An index
  expression must be immutable, while the guard that accepts exactly what the cast accepts
  (`pg_input_is_valid`, the cast's own input function) is only stable. So the index, the sweep read
  comparing its expression and the count of the rows it leaves out share one helper
  (`store.CanonicalUUID`), and every read that resolves the value shares the other
  (`store.CastableUUID`), which answers on every row what the unguarded cast answered. Neither set
  of copies can drift; only the migration keeps a literal, because a migration's text is immutable.
- A `coalesce` whose cast sits behind another column hides which rows a cast ever reached:
  `coalesce(block_artifact_id, (anchor->>'artifact_id')::uuid)` evaluates the cast only where
  `block_artifact_id` is null, so that index proves nothing about an ask that sets both. Read the
  index expression argument by argument before concluding a stored row casts.

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
  enforces this, and rows written by earlier releases are unchecked: an ask holding both columns
  was listed under neither document, so its mark was swept. Since sjawhar/legion#2483 the read no
  longer depends on it: asks are read by the anchor's artifact alone, the same predicate as
  comments, through `asks_anchor_artifact_id` (migration `0148`), an index added for that exact
  expression rather than a predicate bent to fit the index that existed. That index's expression
  guards the cast with a canonical-form regex (`case when anchor->>'artifact_id' ~ '^[0-9a-f]{8}-…$'
  then (anchor->>'artifact_id')::uuid end`), so a stored anchor that does not cast can fail
  neither the migration nor the read: an index cast must not raise on any row a previous schema
  allowed, and here no earlier index had cast the anchor of an ask that also set
  `block_artifact_id`.
- The tester added an anchor with an upper-case artifact id to the equivalence harness. The kept
  text comparison excludes it on both sides.
- That same unguarded cast was in every other reader of an ask's anchor, and `GET
  /api/v1/issues/{key}/asks` answered 500 (`SQLSTATE 22P02`) over such a row on a real server.
  `internal/dispatch/store/canonical_uuid.go` now holds both guards. A reader resolves the anchor
  through `store.CastableUUID` (`case when pg_input_is_valid(x, 'uuid') then x::uuid end`), which
  answers the document every spelling the cast accepts names - upper case, braces, no hyphens, as
  the unguarded cast did - and null only where the cast would raise: `api/ask_rows.go`,
  `api/events.go`, both ask arms of `api/search.go`, `refs/graph.go`'s ask node and
  `docs/marks.go`'s `loadAnchorRefreshedAsk`. `pg_input_is_valid` is only stable, so no index can
  carry it: `store.CanonicalUUID`'s immutable regex stays the expression of
  `asks_anchor_artifact_id` (migration `0148`), the sweep read (`docs/marks.go`'s
  `recordedMarksQuery`) and the boot count (`store.AsksOutsideAnchorArtifactIndex`), so those three
  describe one set of rows. The comment readers stay unguarded: 0032's
  `comments_anchor_artifact` casts every comment row, so no stored comment anchor fails a cast.
- The two search arms read `coalesce(artifact_id, block_artifact_id, <anchor>)`, which never
  reaches the anchor for the rows that do not cast, since they all set `block_artifact_id`; they
  are guarded all the same, so neither arm depends on the order of its own `coalesce` or on an
  index to stay correct.

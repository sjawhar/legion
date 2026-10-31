---
title: "A plan test over a partial index requires an Index Cond on that index"
category: database-issues
tags:
  - postgres
  - explain
  - partial-index
  - testing
date: 2026-10-30
status: active
module: packages/envoy/internal/dispatch/docs
related_issues:
  - "sjawhar/legion#2483"
---

# A plan test over a partial index requires an Index Cond on that index

Extends docs/solutions/database-issues/a-read-reaches-an-expression-index-only-through-its-exact-expression-and-keeps-the-old-predicate-beside-it-pr2467.md.

- When a query's `where` implies a partial index's predicate, the planner can read the whole index
  with no condition on your expression. A plan that names the index and has no `Seq Scan` does not
  prove the expression matched.
- Require the line after the index's node to be an `Index Cond:` line, and forbid `Seq Scan`, on the
  prepared statement under `set local enable_seqscan = off` and
  `set local plan_cache_mode = force_generic_plan`.
- Check the test against a predicate that breaks the match (the text comparison alone) and see it
  fail before you rely on it.

## Evidence

- In sjawhar/legion#2483, `TestRecordedMarkRefsReadsThroughTheAnchorIndexes`
  (`docs/recorded_mark_refs_test.go:111`) first checked only the index name and the absence of a
  `Seq Scan`. With the asks read cut to `anchor->>'artifact_id' = $1`, the plan was `Bitmap Heap Scan
  on asks / Recheck Cond: (anchor IS NOT NULL) / -> Bitmap Index Scan on asks_anchor_artifact_id`,
  with no condition, and the test passed. It now requires the `Index Cond:` line under the index's
  node (`:144`) and fails on that predicate.

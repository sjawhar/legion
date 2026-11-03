---
title: "A batched lookup keeps the row loop and a full ORDER BY, and its test forces heap order against it"
category: database-issues
tags:
  - order-by
  - determinism
  - batching
  - events
date: 2026-10-28
status: active
module: packages/envoy
related_issues:
  - "sjawhar/legion#2447"
---

# A batched lookup keeps the row loop and a full ORDER BY, and its test forces heap order against it

When one batched lookup replaces a lookup per row (one walk, one query or one map in place of N):

- Keep the loop over the rows, and look each row up in the batch's result. Never loop over the
  result map. Map order is random, and two rows that name the same key collapse into one entry,
  but each row must still get its own update and its own event.
- Give the query that lists the rows a total order: `ORDER BY <creation time>, <unique id>`. With
  no `ORDER BY`, Postgres returns heap order. Heap order changes with every update, so one write
  emits its events in a different order on each run.
- Test the order with rows whose creation times sort neither by id nor by insert order. Set those
  times with `UPDATE`s run in the reverse of the wanted order. Each update writes a new row version
  at the end of the heap, so the heap order is then the opposite of the wanted order. Without that
  step, the test can pass with the `ORDER BY` removed.
- Run the refresh for several rounds and check every round. A wrong order matches a five-row
  listing by chance in about one round of five.

## Evidence

- `packages/envoy/internal/dispatch/docs/marks.go:465`: `openAnchoredMarks` gained
  `order by created_at, id`. Another issue's implementer found that `comment.anchor_refreshed` events
  came out in a different order on two runs of one build.
- `refreshAnchors` builds its keys from the rows, calls `pmdoc.FindMarks` once, then loops over the
  rows. `TestRefreshAnchorsRefreshesEveryRowSharingAMark` holds two rows on one mark to two events.
- `TestRefreshAnchorsAnnouncesEveryKindInRowOrder` (`docs/marks_test.go:1566`): its first version
  set the creation times through a Go map. It passed with the `ORDER BY` deleted, because the heap
  order happened to match. With the times set in reverse order, it fails without the `ORDER BY`
  (`round 0: row 0 listed …d2, want …d4`) and passes with it (commit 8279d988).

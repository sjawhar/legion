---
title: "Fold off the lock and write only when every folded row's version and xmin still stand"
category: database-issues
tags:
  - postgres
  - xmin
  - optimistic-concurrency
  - advisory-lock
  - compaction
date: 2026-10-29
status: active
module: packages/envoy
related_issues:
  - "sjawhar/legion#2444"
---

# Fold off the lock and write only when every folded row's version and xmin still stand

- When a step that holds a lock spends its time computing (decoding, folding, rendering), split it.
  Read the rows with `version` and `xmin::text` on a pool of their own, and compute with no lock
  and no connection held.
- Then take the lock for the write alone. In that transaction, read `(version, xmin)` again for
  every row you read, and write only when the two lists are equal. On any difference, discard
  the result and retry later with backoff. Never widen the lock to make the write safe.
- `xmin` changes whenever anything rewrites a row, including an `UPDATE` that leaves its bytes
  as they were, so the check catches every writer, a build that predates the check included.
- Give each way a row can move its own test: a prune, a rebuild, a delete, a second process
  compacting, an older build's compaction, and a settlement that advances a cursor.

## Evidence

- `packages/envoy/internal/dispatch/docs/compaction.go`: `writeCompaction` re-reads the rows
  through `standingRows` (`select version, xmin::text from doc_updates ...`) under `lockRoom`,
  and returns `compactDropped` when they differ from the rows the fold read. It replaces main's
  `Compact`, which held the room's lock and the document's advisory lock for the whole fold, so
  every append to the document waited behind it.
- `docs/compaction_test.go` holds one test per mover listed above. The real-process proof at
  `9ed33934` folded 37 rows, and the 3 young rows kept their version, `xmin` and md5.

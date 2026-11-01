---
title: "Take the lock a writer holds until it marks its work done before you read the queue it marks"
category: best-practices
tags:
  - locking
  - race
  - advisory-lock
  - double-write
date: 2026-11-01
status: active
module: packages/envoy/internal/dispatch/docs
related_issues:
  - "sjawhar/legion#2496"
---

# Take the lock a writer holds until it marks its work done before you read the queue it marks

Extends docs/solutions/database-issues/a-cross-row-one-at-a-time-rule-in-a-where-predicate-is-write-skew-under-read-committed-lock-the-key-first-LEGION-694.md.

- Another writer may commit an item and keep a lock until it marks that item stored in memory. Take that lock first, then read the in-memory queue of unstored items. A read before the lock can see an item the other writer has committed and not marked yet. Your transaction waits on the lock, then stores the item a second time.
- Test the window directly. Hold the other writer between its commit and its mark, run your reader, and assert one stored row. The test must fail when the lock moves back after the read.

## Evidence

- Dispatch's block-id backfill read the room's queue before it took the document's advisory lock. The room's own worker holds that lock in `AppendRoomFlush` from its commit until it marks the update stored. #2496 (commit b67efab8) moves `lockDocumentRoom` right after `Begin`, before `storeRepair` reads the queue (`packages/envoy/internal/dispatch/docs/service.go:2344-2358`).
- `TestStoredOrderBackfillDoesNotStoreTheRoomsCommittedEditTwice` (`backfill_order_test.go:47`) fails with "bob's edit is stored in 2 rows, want 1" when the lock comes after the read.

---
title: "When writers store a producer's commits out of its order, route every writer through one flush and re-measure what the move reorders"
category: envoy
tags:
  - dispatch
  - ordering
  - durable-writes
  - advisory-lock
date: 2026-11-01
status: active
module: packages/envoy/internal/dispatch/docs
related_issues:
  - "sjawhar/legion#2496"
---

# When writers store a producer's commits out of its order, route every writer through one flush and re-measure what the move reorders

- When an in-memory producer (a document room) commits in one order and several writers persist its commits, do not fix each writer's window on its own. Give every writer one primitive. Under the lock it appends under, and before its own append, it stores every unstored commit through the exact position it read the producer at, in the producer's order.
- That position must be exact. Take it under the producer's own lock, together with the state copy it describes, and never hold that lock across a database round trip.
- When a writer's own row is placed ahead of rows already classified against the old prefix (a changed / unchanged flag, a diff, a credit), measure each of those rows again, in stored order, against the prefix it now has. Do this before the first append, and carry whatever the flag gates along with the new value.
- Make the stored order checkable. Fold the stored rows in stored order and compare every derived column and every version with the fold.

## Evidence

- In Dispatch, five separate windows broke the replay. Each was a different writer storing room updates out of commit order: a live write, the version capture, settlement and snapshots, two browsers, and the block-id backfill. #2496 routes all of them through `flushThrough` (`packages/envoy/internal/dispatch/docs/flush.go:17-24`, `:180`; `ledger.go`'s `Ledger.flushThrough`, `flush.go:249`). The room's own worker goes through `PgVersioned.AppendRoomFlush`, and the backfill goes through `storeRepair`.
- The classes of browser updates stored after a live write are measured again against the room's shadow, a state that holds the write (`shadow.go:10-16`, `storedClasses` at `:214`).
- `envoy-dispatch stored-order` (`cmd/dispatch/storedorder.go`, `docs.CheckStoredOrder`) is the check, and a soak of 200 rounds finds 0 mismatches at the head, where every round failed on main.

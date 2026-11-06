---
title: "A caller that does not wait takes the scarce resource before the lock others queue on"
category: database-issues
tags:
  - locks
  - connection-pool
  - concurrency
  - compaction
date: 2026-11-02
status: active
module: packages/envoy/internal/dispatch/docs
related_issues:
  - "sjawhar/legion#2444"
---

# A caller that does not wait takes the scarce resource before the lock others queue on

- A caller that gives up when a lock is busy (a `TryLock`, a `pg_try_advisory_lock`) must not hold
  that lock while it blocks on something else. Take the scarce, blocking resource first (here a
  connection from a small pool), and try the lock only once you hold it. Release the resource if
  the try fails.
- A caller that waits keeps the opposite order: lock first, so the room's callers queue on the
  lock rather than on the pool.
- Pin it with a test that holds every connection of the pool while the non-waiting caller comes
  to acquire one. A writer of the same room must complete while that caller still waits. Wait on
  something observable, not on a sleep: pgxpool counts an acquire only after it succeeds, so this
  test waits on the count of the room lock's users instead.

## Evidence

- `PgVersioned.lockRoom` (`docs/persistence.go`) took the room's in-process lock (`TryLock` when
  not waiting) before a connection from the compaction pool, which has two connections. A queue
  compaction blocked on that pool held the room's lock, and the room's `AppendRoomFlush` and
  `appendUpdate` waited behind it. Review round 15 found this; 95ef7808 fixed it.
- `TestACompactionThatDoesNotWaitHoldsNoRoomLockWhileItWaitsForAConnection`
  (`docs/compaction_test.go`, fccd461a) fails with the old order: "the append never landed while
  the compaction waited for a compaction-pool connection".

---
title: "A pool whose work moves out of its transactions was also a semaphore"
category: database-issues
tags:
  - postgres
  - connection-pool
  - memory-bound
  - concurrency
date: 2026-10-29
status: active
module: packages/envoy
related_issues:
  - "sjawhar/legion#2444"
---

# A pool whose work moves out of its transactions was also a semaphore

Extends docs/solutions/database-issues/a-transaction-never-waits-in-memory-for-work-that-takes-database-locks.md.

- Before you move work out of a pooled transaction (read the rows, end the transaction, then
  compute), name what bounded that work's concurrency and memory. When it ran inside the
  transaction, the pool's size was that bound.
- Replace it with an explicit semaphore, sized from the pool's exported constant rather than by
  a comment. Take a slot within the caller's context, and say in the code why waiting for a slot
  cannot close a cycle with a lock.
- State the memory bound that results (slots × buffer cap) in the code and in the package guide.

## Evidence

- `packages/envoy/internal/dispatch/docs/fold.go`: `bufferedFolds = store.RoomsPoolSize` and
  `foldBufferBytes = 32 << 20`. Its comment says a load once folded inside its transaction on the
  rooms pool, whose connections bounded how many folds ran at once.
- Review round 1 of sjawhar/legion#2444 (thread `PRRT_kwDORFy7ds6xWob8`, fixed in `61cc109a`)
  found that buffered folds had no process-wide bound. Round 2 (`PRRT_kwDORFy7ds6xZA2J`, fixed in
  `299ac590`) found the constant tied to the pool's size only by a comment. `RoomsPoolSize` is now
  exported and named.

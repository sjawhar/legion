---
title: "Work made concurrent on a pool sized for one statement at a time takes a scoped statement gate, so a lease renewal never queues"
category: database-issues
tags:
  - connection-pool
  - concurrency
  - lease
  - renewal
  - pgx
date: 2026-10-31
status: active
module: packages/envoy/internal/dispatch
related_issues:
  - "sjawhar/legion#2395"
---

# Work made concurrent on a pool sized for one statement at a time takes a scoped statement gate, so a lease renewal never queues

- **Before you run a loop's work N at a time, read the reason written at each resource it
  shares.** A pool size, a buffer or a limit sized for "one goroutine, one statement at a time"
  is now wrong, and nothing fails until a renewal or heartbeat waits behind it.
- **Keep a lease's renewal off the queue.** When the renewal shares the pool with the work,
  either give the work one gate (one statement at a time across all its goroutines) so the
  renewal always finds a free connection, or raise the pool and re-sum every copy of the count
  (`docs/solutions/infra/a-copied-connection-count-is-re-summed-from-the-merged-code-at-every-merge-of-main-pr2395.md`).
- **Scope the gate in code, not in a resource's `Close`.** Take it in a function that runs the
  statement, drains its rows and returns (`statement(fn)` with `defer unlock`). Hold it across a
  read that is several statements, so no other worker's statement runs between them. Never hold
  it across a network send. A gate released by `rows.Close()` stalls every worker when one caller
  forgets the close.
- **Fix the lock order.** When the read that takes keys runs under a second mutex, always take
  that one first and the gate second, and let no other path take them the other way.

## Evidence

- `store/pool.go:297-303`: `outboxPoolSize = 2`, sized because "the outbox's scan is one
  goroutine that runs one statement at a time" and the hold's renewal runs beside it. Commit
  `767794be` made the scan run `concurrentBatches = 4` (`outbox/lease.go:36-43`), so four
  batches could hold both connections while the renewal (every 2 s, a 6 s lease) waited.
- `outbox/lease.go:544-548` (`statements`, with what it is held around), `:607-628` (`lock`,
  `unlock`, `statement`, and the order `mu` then `statements`) and `outbox/publisher.go:774-782`
  (`loadLookups` under one gate). Commit `da5762a8` removed the `gated`/`gatedRows` helpers that
  tied the gate's release to closing a result set.
- Nearest document: `docs/solutions/database-issues/a-transaction-never-waits-in-memory-for-work-that-takes-database-locks.md`
  is about an in-memory wait inside a transaction, not a renewal starved on a pool by new
  concurrency.

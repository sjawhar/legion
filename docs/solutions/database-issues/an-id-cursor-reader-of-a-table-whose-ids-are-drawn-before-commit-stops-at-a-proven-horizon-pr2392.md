---
title: "An id-cursor reader of a table whose ids are drawn before commit stops at a proven horizon"
category: database-issues
tags:
  - postgres
  - event-log
  - cursor
  - snapshot
  - pg_locks
date: 2026-10-30
status: active
module: packages/envoy/internal/dispatch/events
related_issues:
  - "sjawhar/legion#2392"
---

# An id-cursor reader of a table whose ids are drawn before commit stops at a proven horizon

- A table whose ids a sequence hands out before commit (`bigserial`, `nextval`) commits out of id order once writers stop waiting on one shared lock. A reader that pages it by `id > cursor` can then skip a row that commits later with a lower id. Such a reader reads only up to an id proven final, never up to `max(id)`.
- To prove an id final from Postgres's own bookkeeping, every writer must take its transaction id before it draws the row's id. Take the transaction id inside a statement the writer already runs (`pg_current_xact_id()` in its `select ... for no key update`), which costs no extra round trip.
- To find another backend's transaction id without superuser rights, read `pg_locks`: it shows every backend's locks to every role, while `pg_stat_activity.backend_xid` does not. To learn which transactions can still write the table, join the relation lock on that table with the `transactionid` lock. That ignores open transactions that hold an id but write nothing, such as an operator's `psql`.
- When one process stores such a proven id for another process to trust, stamp it with what the proving snapshot read: the row's `created_at` from the same statement. Make the write conditional on the row still carrying that value. Reading `created_at` at write time instead lets a reset of the table (`truncate ... restart identity`) pair an old id with a new row.

## Evidence

In sjawhar/legion#2392, removing the database-wide advisory lock from `events.Broker.Append` made event ids commit out of order across issues. Every global-cursor reader (the live feed's relay, the delivery hub, `GET /api/v1/references?since=`) now reads up to `events.Horizon`'s safe horizon. The proof is in the package comment of `packages/envoy/internal/dispatch/events/horizon.go`. `lockEventOwner` in `events/broker.go` takes the writer's transaction id in its row-lock statement, and `oldestEventWriterSQL` (`horizon.go:434`) joins `pg_locks` on the `events` relation to `transactionid`.

`persistHorizonSQL` (`horizon.go:122`) first read the `created_at` of the row at H when it wrote. Commit `185bb85d5936` makes each observation read it in its own statement and writes only `where e.id = $1 and e.created_at = $2`. `TestAPersistOfAnOldLogsHorizonWritesNothingAfterTheLogIsReset` proves it.

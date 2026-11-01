---
title: "A migration on a hot table builds its indexes before any ACCESS EXCLUSIVE statement, and each partial predicate is spelled as the query spells it"
category: database-issues
tags:
  - postgres
  - migration
  - partial-index
  - locks
  - query-planner
date: 2026-10-28
status: active
module: packages/envoy/internal/dispatch/store
related_issues:
  - "sjawhar/legion#2395"
---

# A migration on a hot table builds its indexes before any ACCESS EXCLUSIVE statement, and each partial predicate is spelled as the query spells it

- **Order the statements by lock.** A Dispatch migration runs in one transaction, so every lock is
  held until commit. `create index` (without `concurrently`) takes SHARE: writes wait, reads go on.
  `alter table … add column` and `drop index` take ACCESS EXCLUSIVE: reads wait too. Put the index
  builds first and the ACCESS EXCLUSIVE statements last, so reads wait only for the tail.
- **Spell a partial index's predicate exactly as every query that should use it spells it.**
  Postgres uses a partial index only when it can prove the predicate from the query's own
  expression; an expression with the same meaning written another way
  (`coalesce(payload ->> 'pending', 'false') <> 'true'` against
  `(payload ->> 'pending') is distinct from 'true'`) silently falls back to a scan.
- **State what dropping an index costs the release before.** A rolling update runs the old build on
  the new schema; read its queries against the new indexes with `EXPLAIN` and say what it costs.
- **After a window-function CTE, fetch wide rows with a lateral primary-key lookup.** A plain
  `join events e on e.id = r.id` after the filter can be planned as a merge join over the whole
  primary key; `cross join lateral (select * from events where id = r.id limit 1) e` is not
  flattened, so it stays one index lookup per kept row.

## Evidence

- `packages/envoy/internal/dispatch/store/migrations/20261101143203_outbox_claim.up.sql:34-46`: three index
  builds, then the column add and the two drops. Built on 1,000,000 and 5,000,000 event rows in
  1.1 s and 7.2 s; with the column add first, reads of `events` would have waited all of it.
- `20261101143203_outbox_claim.up.sql:7` and `:35-41`: one spelling in every predicate and in
  `outbox/batch.go`'s queries. Main's old claim, with the `coalesce` spelling, reads `events` by a
  sequential scan after this migration (125 ms on 1,000,000 rows) where it used the dropped
  `events_unpublished` (3.4 ms).
- `packages/envoy/internal/dispatch/outbox/batch.go:139`: the plain join took 5.19 ms and failed
  `TestTheClaimSkipsPendingCommandsAndBackedOffRows`'s 0.5 ms bound; the lateral lookup took
  0.41-0.44 ms.

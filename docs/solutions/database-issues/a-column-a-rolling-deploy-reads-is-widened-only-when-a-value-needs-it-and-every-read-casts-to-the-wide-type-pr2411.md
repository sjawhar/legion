---
title: "A column a rolling deploy reads is widened only when a value needs it, and every read casts to the wide type"
category: database-issues
tags:
  - postgres
  - pgx
  - rolling-deploy
  - migration
  - prepared-statements
date: 2026-10-31
status: active
module: packages/envoy/internal/dispatch/store
related_issues:
  - "sjawhar/legion#2411"
---

# A column a rolling deploy reads is widened only when a value needs it, and every read casts to the wide type

- Changing a column's type while the previous release still serves breaks that release's prepared reads.
  pgx caches each statement's plan per connection, and Postgres refuses a cached plan whose result type changed: `cached plan must not change result type`.
  An old pod fails one read per connection after the new pod's boot alters the column.
- Do not alter the column at every boot.
  Alter it only in the boot whose data first needs the wider type (here, the first migration version above `math.MaxInt32`), inside the same locked transaction that records that value.
  A release that only ships the ability to widen then rolls over the previous one without touching the column.
- In the release that can widen, cast every read and write of the column to the wide type (`max(version)::bigint`, `$1::bigint`), so its own plans keep one result type before and after the widening.
- Say in the release notes that this release must roll out before any release that carries a value which triggers the widening: the previous release's reads are uncast.
- Prove it with two real pods on one database: the previous release reading in a loop while the new one boots, once with no wide value (the column must stay narrow, 0 failed reads) and once with one (the column widens, 0 failed reads from the new release).

## Evidence

sjawhar/legion#2411 moved new migrations from `0150_…` to `YYYYMMDDHHMMSS_…` versions, which need `bigint`.
Its first form widened `schema_migrations.version` at every boot (`c635b7a9`): main's pod, reading `Pool.Healthy` every 50 ms, failed 1 of 41 reads with `cached plan must not change result type`.
The fix widens only when the set holds a version past an integer (`widenSchemaVersion` in `packages/daemon/internal/store/store.go`, `createVersionTable(ctx, widen)` in `packages/envoy/internal/dispatch/store/store.go`) and casts the health read (`pool.go`, `coalesce(max(version), 0)::bigint`).
Both rolls then read 41 of 41.

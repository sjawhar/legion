---
title: "A SQL function in a predicate runs once per row, so index every table it reads"
category: database-issues
tags:
  - postgres
  - sql-function
  - index
  - explain
date: 2026-10-27
status: active
module: packages/envoy
related_issues:
  - "INFRA-487"
  - "sjawhar/legion#2269"
---

# A SQL function in a predicate runs once per row, so index every table it reads

- Postgres does not inline a `language sql` function whose body has a subquery. Called in a
  `where` clause or select list, it runs once for every row the statement evaluates, so a lookup
  inside it that is cheap once becomes a scan per row.
- Index every table such a function reads for the exact lookup it makes (for
  `order by last_serving_at desc limit 1`, an index on `(last_serving_at desc)`), even when the table
  is small today: count the rows it can hold at its worst, not at its usual.
- Prove it two ways, each with the index dropped as the negative control: `EXPLAIN ANALYZE` of the
  function's inner select (an `Index Scan`, not `Seq Scan` + `Sort`), and the time of N calls whose
  argument changes per row. An argument that does not depend on the row is folded into a one-time
  filter (`One-Time Filter` in the plan), which times one call, not N.

## Evidence

`enrollment_lease_live` (`packages/envoy/internal/broker/store/migrations/0019_broker_serving.up.sql`)
reads the latest `broker_serving` row and runs at nine liveness sites, the sweep's selection among
them. With 5,050 rows (one per process, kept up to an hour), 20,000 calls
(`select count(*) from generate_series(1,20000) g where not enrollment_lease_live(now() - interval '1 minute' - g * interval '1 microsecond')`)
took 35,109 ms without an index and 301 ms with `broker_serving_latest` (commit e847db82, found in
review round 2). The same query with a constant argument took about 10 ms either way, because the
planner evaluated it once.

---
title: "Postgres 18 hashes a NOT EXISTS over a full scan, so a per-row absence check is a scalar subquery"
category: database-issues
tags:
  - postgres
  - postgres-18
  - query-plan
  - not-exists
  - index
date: 2026-10-28
status: active
module: packages/envoy/internal/dispatch
---

# Postgres 18 hashes a NOT EXISTS over a full scan, so a per-row absence check is a scalar subquery

- To check that a candidate row has no matching row elsewhere, with one index probe per
  candidate, write `(select 1 from t where … limit 1) is null`, not `not exists (select 1 from t
  where …)`. Postgres may run a `NOT EXISTS` (an EXISTS/ANY sublink) as a hashed subplan, whose hash
  table it builds from a full scan of `t`. It never hashes a scalar subquery: that runs once per
  outer row, each run one index probe.
- On Postgres 18, `set enable_seqscan = off` does not forbid that scan: the plan can still hold
  `Seq Scan on t … Disabled: true` under the hashed subplan. Judge the plan node, not the setting.
- A plan test green on 16 is no proof for 18, and a test run with `enable_seqscan = off` is no
  proof for default settings. Run `EXPLAIN` on both majors with the setting off and on, and keep
  all four plans.
- Fix the query, never the plan test.

## Evidence

- `owedCopiesQuery` (`packages/envoy/internal/dispatch/docs/copied_asks.go`, the comment at
  211-215 and the `limit 1 ) is null` form at 228-232) checked a copy's own ask with `NOT EXISTS`.
  `TestCopiedAskQueriesAvoidSequentialScans` (`copied_asks_plan_test.go`, unchanged) passed on
  PostgreSQL 16.15 and failed on 18.6 with `query plan sequentially scans asks; want
  asks_block_id_unique`: the plan was `NOT (ANY ((d.id = (hashed SubPlan 2).col1) …))` over
  `Seq Scan on asks a`, `Disabled: true`.
- On 16 with default settings the same query also planned `NOT (hashed SubPlan 2)` over
  `Seq Scan on asks a`; only the test's `enable_seqscan = off` hid it.
- The scalar-subquery form (commit 834b17a9) plans `((SubPlan 1) IS NULL)` over
  `Limit -> Index Scan using asks_block_id_unique` on both majors under both settings. All eight
  plans are quoted on sjawhar/legion#2364 (comment 6440785283).

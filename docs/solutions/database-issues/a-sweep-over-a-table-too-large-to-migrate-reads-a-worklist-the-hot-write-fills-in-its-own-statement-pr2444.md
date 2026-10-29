---
title: "A sweep over a table too large to migrate reads a worklist the hot write fills in its own statement"
category: database-issues
tags:
  - postgres
  - migration
  - census
  - periodic-sweep
  - cte
date: 2026-10-29
status: active
module: packages/envoy
related_issues:
  - "sjawhar/legion#2444"
---

# A sweep over a table too large to migrate reads a worklist the hot write fills in its own statement

- Do not let a periodic pass scan or `group by` a hot Dispatch table. Do not plan an index or a
  column on it either: the pre-deploy census refuses a migration that names a table of 1 GiB or
  more (`pgmigrate.CensusTableLimit`, `internal/pgmigrate/census.go`).
- Create a small keyed table, one row per item the pass owes work on, with no foreign key, so
  that a write naming it locks nothing else.
- Have the hot write add the row in a CTE of the statement it already runs
  (`insert ... on conflict do nothing`), so it costs no extra round trip. Delete the row in each
  transaction that makes it moot.
- Backfill the new table in its migration by reading the large table under `ACCESS SHARE` only.
  Say in the README what the release before it fails to write during the roll.

## Evidence

- `packages/envoy/internal/dispatch/store/migrations/0141_doc_compaction_owed.up.sql`: the table
  and its one-time backfill. `docs/persistence.go`: the append's `owed as (insert into
  doc_compaction_owed ...)` CTE. `docs/compaction.go`, `docs/persistence.go` (rebuild, delete)
  and `docs/compaction_sweep.go` remove the row.
- On 100,000 one-row documents, the old listing
  (`select artifact_id from doc_updates group by artifact_id having count(*) > 1`) took 42 ms and
  read 624 buffers per hourly pass. The table's listing took 0.054 ms. The append's latency
  stayed within run-to-run spread over 5 harness runs per side.

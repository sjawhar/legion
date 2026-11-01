-- 0159_search_term_totals_snapshot.up.sql
-- What a refresh pass of this release writes beside a project's item count, and what tells a
-- later pass whether that count is its own work.
--
-- Every pass of the new code, whether it rebuilds the project in full or applies 0158's queue,
-- writes items, refreshed_at, snapshot and snapshot_at in the one statement that counts:
-- refreshed_at and snapshot_at both from that statement's statement_timestamp(), and snapshot
-- from the pg_current_snapshot() it takes there. A pass then drops exactly the queue rows that
-- snapshot sees, so the counts and the drained rows describe one moment.
--
-- The signal that somebody else wrote the row is refreshed_at is distinct from snapshot_at, not a
-- null snapshot. The release before this one updates items and refreshed_at and nothing else
-- (api/related_terms.go searchTermStatsRebuild), so during a roll its rebuild leaves this
-- release's snapshot in place while it moves refreshed_at, and a rule keyed on a null snapshot
-- would read the row as this release's own and apply the queued changes that rebuild had already
-- counted. A pass therefore rebuilds a project in full when snapshot is null, which is a project
-- no pass of this release has ever written, or when refreshed_at is distinct from snapshot_at,
-- and its rebuild drops the queue rows its own snapshot sees.
--
-- Both columns are nullable and have no default, so adding them writes pg_attribute and rewrites
-- no row: the table holds one row per project, and the rows already there read as the null
-- snapshot of a project this release has not yet counted, which is what makes the first pass
-- after the deploy a full build.
alter table search_term_totals
  add column snapshot pg_snapshot,
  add column snapshot_at timestamptz;

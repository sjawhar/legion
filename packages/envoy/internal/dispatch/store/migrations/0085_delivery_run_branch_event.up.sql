-- LEGION-567 slice 2: GitHub's head_branch and event on every stored run. The delivery measures
-- and the timeline count deploy runs on main; the Pipeline page counts push runs on main and
-- pull_request PR-check runs. Both are null on a row stored before these columns existed, and a
-- null counts in nothing: a resumable backfill step per run kind (delivery.Reconcile.backfillRuns,
-- progress row 'backfill/runs/<kind>') re-lists the last 28 days once, filling them; no row is
-- rewritten here.
alter table delivery_runs
  add column head_branch text,
  add column event text;
-- ListRunsStartedIn (the measures and the Pipeline) filters and orders by started_at.
create index delivery_runs_kind_started_at on delivery_runs (repo, kind, started_at);
-- The backfill step records when it began, so it knows when it has caught up with itself; the
-- regular steps leave this null.
alter table delivery_reconcile_progress add column began_at timestamptz;

-- 0081_delivery_runs.up.sql
-- LEGION-567: deploy-workflow and PR-checks workflow runs, with their jobs, on the configured
-- deploy repository. One shape covers both kinds (kind = 'deploy' | 'pr_checks') since the
-- containment algorithm reads only deploy-kind rows' jobs by name, and the later Pipeline page
-- (not this slice) reads PR-checks timing from the same shape: a second near-identical table
-- would only duplicate the columns both need. pr_number is set for pr_checks runs, null for
-- deploy runs (which run on pushes to main, not on a pull request).
--
-- Both new tables, created together: no trigger or index here touches an existing table, so
-- there is no reason to split them further.
create table delivery_runs (
  repo text not null,
  run_id bigint not null,
  kind text not null check (kind in ('deploy', 'pr_checks')),
  pr_number integer,
  head_sha text not null,
  head_commit_at timestamptz not null,
  started_at timestamptz not null,
  completed_at timestamptz,
  conclusion text check (conclusion in ('success', 'failure', 'cancelled')),
  url text not null,
  primary key (repo, run_id)
);

-- Indexed on head_commit_at, not started_at: store.go's ListRuns (containment's input and the
-- timeline handler's lookback query) filters and orders by head_commit_at, never started_at.
create index delivery_runs_kind_started on delivery_runs (repo, kind, head_commit_at);

-- A job's own identity within a run is its name: GitHub does not number jobs, and a run never
-- repeats a job name within one attempt (the latest attempt is what intake and reconcile keep).
create table delivery_run_jobs (
  repo text not null,
  run_id bigint not null,
  name text not null,
  started_at timestamptz,
  completed_at timestamptz,
  conclusion text check (conclusion in ('success', 'failure', 'cancelled', 'skipped', 'timed_out')),
  primary key (repo, run_id, name),
  foreign key (repo, run_id) references delivery_runs (repo, run_id) on delete cascade
);

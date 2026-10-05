-- 0072_delivery_settings.up.sql
-- LEGION-567: the one configuration record the delivery timeline's intake, reconcile and API
-- read: which repository deploys, its deploy and PR-checks workflow paths, the production job
-- name, and the population rule's authors and excluded repositories (LEGION-294's rule, kept out
-- of code since this repository is public). At most one row: the first singleton table in this
-- schema, enforced the standard Postgres way (a fixed primary key plus a check that it holds that
-- value), since every other table here is keyed by project/issue/artifact/user/session id and
-- none of that fits a value nothing else owns.
--
-- last_event_at and last_reconcile_at are the Delivery page's freshness row: the last GitHub
-- event the intake consumer processed, and the last time the five-minute reconcile *completed
-- successfully*. They live here rather than a fourth table because they are exactly one row of
-- operational state with no identity of their own. last_reconcile_at does not advance on a failed
-- pass (a missing permission, a rate limit, any other GitHub or store error): last_error carries
-- that pass's failure instead, cleared the next time a pass succeeds, so the freshness row can
-- distinguish "stale because nothing's happened" from "stale because something's broken" instead
-- of reporting a healthy timestamp for a reconcile that silently stopped doing anything.
create table delivery_settings (
  singleton boolean primary key default true check (singleton),
  deploy_repo text not null,
  deploy_workflow_path text not null,
  production_job_name text not null,
  pr_checks_workflow_path text not null,
  population_authors text[] not null default '{}',
  excluded_repos text[] not null default '{}',
  last_event_at timestamptz,
  last_reconcile_at timestamptz,
  last_error text,
  updated_by jsonb not null,
  updated_at timestamptz not null default now()
);

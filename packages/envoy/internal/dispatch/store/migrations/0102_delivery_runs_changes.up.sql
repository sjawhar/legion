-- 0102: whether a run's stored jobs were read after the run ended, and delivery_runs' change trigger
-- (0100). Rows already stored were read then (intake and the reconcile read jobs only for a concluded
-- run), so they take true, which Postgres records without rewriting a row; a run written from now on
-- starts false. One migration for one table's lock.
alter table delivery_runs add column jobs_complete boolean not null default true;
alter table delivery_runs alter column jobs_complete set default false;
create trigger delivery_runs_changed after insert or update or delete on delivery_runs
  for each row execute function delivery_runs_changed();

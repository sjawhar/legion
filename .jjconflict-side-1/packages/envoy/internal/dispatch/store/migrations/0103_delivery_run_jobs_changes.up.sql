-- 0103: delivery_run_jobs logs its changes (0100). Its own migration, so the transaction holds this
-- table's lock alone, for milliseconds, and never while waiting for another table's.
create trigger delivery_run_jobs_changed after insert or update or delete on delivery_run_jobs
  for each row execute function delivery_run_jobs_changed();

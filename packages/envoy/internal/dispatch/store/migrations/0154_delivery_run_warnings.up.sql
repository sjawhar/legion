-- 0154_delivery_run_warnings.up.sql
-- The configuration warnings the Delivery page's reads log once across every Dispatch
-- process. A read judges every deploy run (api/delivery.go, productionApplies) and warns about a
-- successful run holding none of delivery_settings.production_job_names, and about a run whose
-- production job lists no step named production_apply_step_name: either run never counts as a
-- deploy. Each process kept the runs it had warned about to itself, so every process serving reads
-- logged each warning once more. Now a process inserts the warning's row before it logs, and only
-- the process whose insert wrote the row logs; each still remembers its own warnings in memory, so
-- it tries the insert once per run and configuration.
--
-- One row per warning logged:
--   repo, run_id  the deploy run warned about.
--   job_names     the production job names configured when it was warned about.
--   apply_step    the configured apply step for a run whose production job lists no step of that
--                 name; '' for a run holding no production job.
-- A changed list or step is a new row, so a misconfiguration is named again under its new value.
-- Nothing deletes a row: one is written per misconfigured run and configuration, as each process's
-- memory held one before.
--
-- A new table: nothing existing is read, locked or rewritten, and the release before this one
-- neither reads nor writes it, so it rolls beside this one (cmd/dispatch/README.md, "Database
-- migrations"), each of its processes logging its own warnings as before. The census answers 0,
-- and rolling back past it is an image rollback alone.
create table delivery_run_warnings (
    repo text not null,
    run_id bigint not null,
    job_names text[] not null,
    apply_step text not null,
    primary key (repo, run_id, job_names, apply_step)
);

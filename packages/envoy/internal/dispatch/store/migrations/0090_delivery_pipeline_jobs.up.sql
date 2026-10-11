-- LEGION-567 slice 3: the Pipeline page follows three more jobs by name, the deploy workflow's
-- staging end-to-end gate and staging deploy and the PR-checks workflow's roll-up job, which name
-- the deployment's jobs and so are settings beside production_job_name, not code. Each may be
-- null: a pipeline without that step omits its card, and no roll-up means nothing is left out of
-- the failing-jobs table.
alter table delivery_settings
  add column staging_e2e_job_name text,
  add column staging_apply_job_name text,
  add column pr_checks_rollup_job_name text;

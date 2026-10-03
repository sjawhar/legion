-- 0017_deaths_and_interrupted_tasks.up.sql — what a claim's process dying with work outstanding
-- leaves behind.
--
-- claims.deaths is supervise.Budgets.Deaths: processes that died after their agent was ready
-- while it held a pending task, counted against that task until it ends (its turn ends, or a
-- suspension or phase change retires it) and bounded by the launch failure limit. Zero for every
-- existing claim, which has counted none.
--
-- pending_task_deliveries.interrupted is supervise.Delivery.Interrupted: a turn of the task was
-- running when its process died, so it is re-sent behind the sentence saying so. False for every
-- existing delivery, which no death has interrupted.
alter table claims add column deaths integer not null default 0 check (deaths >= 0);
alter table pending_task_deliveries add column interrupted boolean not null default false;

-- 0017_claim_deaths.up.sql — a claim's deaths with work outstanding (supervise.Budgets.Deaths):
-- processes that died after their agent was ready while it held a pending task, counted until
-- the agent next completes a turn and bounded by the launch failure limit. Zero for every
-- existing claim, which has counted none.
alter table claims add column deaths integer not null default 0 check (deaths >= 0);

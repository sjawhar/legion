-- 0062_claim_suspended_by_idle.up.sql — the idle suspend's source on a claim
-- (supervise.SuspendedByIdle, 'idle'): a worker that had nothing to do for
-- worker_idle_suspend_seconds, whose process the daemon took back and whose session it kept. It is
-- the daemon's own suspension by rule, as 'daemon' and a root's 'exit' are, so the boot's resume by
-- rule, the wakes and the stranded report act on it as they act on those.
--
-- 0038's check names the four sources that release had, and Postgres refuses every row the idle
-- suspend writes until the check names this one too. The column keeps its name, its type, its
-- default and every row it holds: only the check is replaced.
alter table claims drop constraint claims_suspended_by_check;
alter table claims add constraint claims_suspended_by_check
  check (suspended_by in ('', 'unknown', 'operator', 'daemon', 'exit', 'idle'));

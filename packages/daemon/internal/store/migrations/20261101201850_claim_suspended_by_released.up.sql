-- 20261101201850_claim_suspended_by_released.up.sql — 'released' joins the sources of
-- supervise.Claim.SuspendedBy: the daemon released the claim's idle issue pod, whose disk and
-- Sandbox went with it and whose unpushed work was saved as a git bundle. The claim keeps its
-- phase and its pending delivery, and the next wake for its role resumes the same session; the
-- daemon's boot resumes no released claim. 0038 wrote the check with the column, so Postgres named
-- it claims_suspended_by_check; it is dropped and written again with the new value. No row holds
-- it yet, so there is nothing to backfill.
alter table claims drop constraint if exists claims_suspended_by_check;
alter table claims add constraint claims_suspended_by_check
  check (suspended_by in ('', 'unknown', 'operator', 'daemon', 'exit', 'released'));

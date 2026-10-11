-- 0038_claim_suspended_by.up.sql — who suspended a suspended claim (supervise.Claim.SuspendedBy):
-- 'operator' (the operator's suspend route), 'daemon' (the workflow's own suspend: the issue left
-- the workflow, or its tree lingers) or 'exit' (the tree root's agent reported its own exit). The
-- daemon resumes a suspended claim by rule unless an operator suspended it; its boot resumes only a
-- 'daemon' or 'exit' one.
alter table claims add column suspended_by text not null default ''
  check (suspended_by in ('', 'unknown', 'operator', 'daemon', 'exit'));

-- Every suspended claim that predates this column is 'unknown'. Most are the daemon's own, which
-- the triggers main had (a phase's start, re-admission, the controller keeper) still resume; the
-- boot's resume by rule, which main did not have, leaves them alone, so it never overrides a drain
-- made before the upgrade.
update claims set suspended_by = 'unknown' where state = 'suspended';

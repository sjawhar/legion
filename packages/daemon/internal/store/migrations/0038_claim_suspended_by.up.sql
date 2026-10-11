-- 0038_claim_suspended_by.up.sql — who suspended a suspended claim (supervise.Claim.SuspendedBy):
-- 'operator' (the operator's suspend route), 'daemon' (the workflow's own suspend: the issue left
-- the workflow, or its tree lingers) or 'exit' (the tree root's agent reported its own exit). The
-- daemon resumes a suspended claim by rule — when it owns its issue's current phase, or a notice
-- waits for it — only for the known daemon and exit sources. An operator's suspension, and a
-- pre-migration one whose source cannot be known, hold until the operator resumes the claim.
alter table claims add column suspended_by text not null default ''
  check (suspended_by in ('', 'unknown', 'operator', 'daemon', 'exit'));

-- Every suspended claim that predates this column is conservatively unknown. In particular, a
-- release drain made before this migration must not be resumed by rule on the first upgraded boot.
update claims set suspended_by = 'unknown' where state = 'suspended';

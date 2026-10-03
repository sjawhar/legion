-- 0008_claim_workspace_lost.up.sql — a claim whose session was lost with its tree volume.
--
-- workspace_lost is set when the claim's own workspace-init, or another claim's of its tree, found
-- the tree volume lost: the claim records no session, and every launch recreates its workspace,
-- fresh, until a new agent registers.
alter table claims add column workspace_lost boolean not null default false;

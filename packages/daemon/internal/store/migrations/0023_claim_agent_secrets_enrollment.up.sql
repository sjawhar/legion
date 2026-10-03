-- 0023_claim_agent_secrets_enrollment.up.sql — the secrets broker's enrollment of a claim's process.
--
-- A pod the daemon enrolls with the secrets broker (AGENTC-393) has an enrollment id the daemon
-- must revoke when it lets the pod go, on whichever daemon observes that. The claim records the
-- id and the incarnation (pod uid) it was made for; a record for another incarnation is stale
-- and only revoked, never used. Both null for a claim with no enrolled process, and set together.
alter table claims add column agent_secrets_enrollment text;
alter table claims add column agent_secrets_enrollment_incarnation text;
alter table claims add constraint claims_agent_secrets_enrollment_pair check (
  (agent_secrets_enrollment is null) = (agent_secrets_enrollment_incarnation is null));

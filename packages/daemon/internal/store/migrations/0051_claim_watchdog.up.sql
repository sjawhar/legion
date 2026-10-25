-- 0051_claim_watchdog.up.sql — the turn watchdog's position on a claim whose agent's turn stopped
-- producing events (supervise.Claim.Watchdog, internal/supervise/watchdog.go): the rung the ladder
-- last ran, the silence it started at, when that rung ran, and every rung with what it found. Null
-- for a claim the ladder has never acted on and for one whose agent is working again, which is
-- every claim at this migration: a daemon that restarts mid-ladder reads the record and takes the
-- ladder up where it left it rather than interrupting a fresh agent.
alter table claims add column watchdog jsonb;

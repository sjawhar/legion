-- 0036_claim_capabilities.up.sql — each session's report of the live capability rows (LEGION-663):
-- what it measured of subagents, web search, MCP, repository extensions, the Dispatch and Envoy
-- tools and GitHub at its start, sent with its ready and kept so a daemon that restarts still
-- renders the sessions it re-adopts.
--
-- One row per claim, the latest report: a relaunched claim reports anew, and the earlier report
-- said nothing of the process now running. The locator is the reporting process's, in the same
-- encoding as claims.locator, and its incarnation is copied out so a reader fences a stale report
-- against the claim's current locator without decoding the document. The report document is
-- {"elapsedMs": n, "rows": [{"name", "ok", "detail"}]}, the rows in the capability table's order.
-- A claim's row goes with the claim.
create table claim_capabilities (
  claim_token text primary key references claims (token) on delete cascade,
  generation bigint not null,
  incarnation text not null,
  locator jsonb not null,
  measured_at timestamptz not null,
  reported_at timestamptz not null,
  report jsonb not null
);

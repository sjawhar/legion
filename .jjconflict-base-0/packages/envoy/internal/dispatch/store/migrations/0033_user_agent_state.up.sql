-- 0033_user_agent_state.up.sql
-- A human's per-agent view state on the Agents page: `Clear conversation` records the moment
-- of the clear, and the page hides every exchange whose newest message is at or before it for
-- that viewer only. The messages themselves are untouched; the row is per (login, session), so
-- the clear follows the human across devices the way user_issue_state does for issues.
create table user_agent_state (
  login text not null,
  session_id text not null,
  cleared_before timestamptz not null,
  primary key (login, session_id)
);

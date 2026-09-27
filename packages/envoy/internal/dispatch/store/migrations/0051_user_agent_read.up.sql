-- 0051_user_agent_read.up.sql
-- How far a human has read a session's conversation on the Agents page. A session's reply to a
-- message the human sent that is newer than both read_through and the human's Clear
-- (user_agent_state.cleared_before) is unread for that human, which is how Dispatch tells them an
-- agent answered without their having to open the conversation to find out.
--
-- A table of its own rather than a column on user_agent_state: a viewer who has only read a
-- conversation has a read mark and no Clear, and user_agent_state requires cleared_before, which
-- a Dispatch that predates this table scans as a timestamp. Per (login, session) like
-- user_agent_state, so the mark follows the human across devices.
create table user_agent_read (
  login text not null,
  session_id text not null,
  read_through timestamptz not null,
  primary key (login, session_id)
);

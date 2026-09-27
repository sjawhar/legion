-- 0051_user_agent_read_through.up.sql
-- How far a human has read a session's conversation on the Agents page. A session's reply to a
-- message the human sent that is newer than both read_through and the human's Clear
-- (cleared_before) is unread for that human, which is how Dispatch tells them an agent answered
-- without their having to open the conversation to find out. A viewer who has only read a
-- conversation has a row with no Clear, so cleared_before is no longer required.
alter table user_agent_state
  add column read_through timestamptz,
  alter column cleared_before drop not null;

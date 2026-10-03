-- 0064_user_agent_reply_read.up.sql
-- The replies a human has read one by one, beside the per-session read mark (user_agent_read). A
-- read mark covers every reply up to a moment, which is right where a whole conversation is on
-- screen (an Agents row, the live view) and wrong where only some of a session's replies are: a
-- broadcast page shows each recipient's reply to that broadcast alone, and moving the session's
-- mark through it would also mark read an older reply to another message the human never saw. A
-- reply named here is read; every other reply is still judged against the session's mark
-- (api/unread_replies.go's unreadDirectRepliesCTE).
--
-- The login is canonical (lower case), like user_agent_read's (0051), so one person has one set
-- however their identity source spells their login. No foreign key to messages: the write checks
-- that each id is the session's own message, and a key would only lock messages for a table that
-- is empty when it is created.
create table user_agent_reply_read (
  login text not null,
  reply_id uuid not null,
  primary key (login, reply_id)
);

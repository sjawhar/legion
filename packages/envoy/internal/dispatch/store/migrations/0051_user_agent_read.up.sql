-- 0051_user_agent_read.up.sql
-- How far a human has read a session's conversation on the Agents page. A session's reply to a
-- message the human sent that is newer than both read_through and the human's Clear
-- (user_agent_state.cleared_before) is unread for that human, which is how Dispatch tells them an
-- agent answered without their having to open the conversation to find out.
--
-- A table of its own rather than a column on user_agent_state: a viewer who has only read a
-- conversation has a read mark and no Clear, and user_agent_state requires cleared_before, which
-- a Dispatch that predates this table scans as a timestamp. Per (login, session) like
-- user_agent_state, so the mark follows the human across devices. The login is canonical (lower
-- case), like user_ask_snooze's (0048), so one person has one read mark however their identity
-- source spells their login.
create table user_agent_read (
  login text not null,
  session_id text not null,
  read_through timestamptz not null,
  primary key (login, session_id)
);

-- Every direct conversation that already exists is read as of this migration. Before it there
-- was no read mark to count against, so without this every reply ever stored would turn unread
-- at the deploy, for sessions long gone as well as live ones. The keys match the unread count's
-- own: the human who wrote an issue-less root (lower(author->>'id'), the canonical login) and the
-- session it targets. A reply stored after this moment, by either image during a rolling deploy,
-- is newer than the mark and counts.
insert into user_agent_read (login, session_id, read_through)
select lower(author->>'id'), substr(target, length('session:') + 1), now()
from messages
where issue_key is null and in_reply_to is null and target like 'session:%'
  and author->>'kind' = 'user'
group by 1, 2;

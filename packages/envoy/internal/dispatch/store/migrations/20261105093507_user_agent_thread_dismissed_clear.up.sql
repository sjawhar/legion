-- 20261105093507_user_agent_thread_dismissed_clear.up.sql
--
-- INFRA-450: the Agent half's own "a write by somebody else brings the thread back", as a trigger,
-- which is what user_threads_record does for a Reply thread (20261105093501). Until this migration
-- the Agent half had no trigger at all, so "dismissed" was a comparison every read had to make -
-- is this mark at or after the newest reply the session wrote - and the Inbox's reads had to ask it
-- of every thread the person had ever dismissed: one index read a thread, 66 ms of the Inbox read's
-- 69 at 10,000 dismissed Agent threads, growing with the pile for as long as it grows
-- (INFRA-450's a14). With the mark cleared in the reply's own transaction, "dismissed" on this half
-- is what it is on the other one - a mark exists - and no read compares two times or visits a
-- dismissed root at all.
--
-- Somebody else, on this half, is the session: an Agent row is a one-off message the person sent a
-- session, and the only writes the Inbox reads in that thread are the session's own replies
-- (api/replies_agent.go). So the trigger fires on a session's write and on nothing else, which is what
-- leaves the person's own reply from the Dismissed view with the thread still dismissed, exactly as
-- their own reply leaves a Reply thread's mark where it stands.
--
-- The delete takes the root's mark whatever its time: it compares the mark with nothing. A
-- trigger runs once, at this insert and inside the writing transaction, so every mark it can
-- reach was written before this reply committed, by a dismiss that cannot have seen it. A
-- dismiss that had seen this reply is turned away before it writes a mark at all, by the check it
-- makes under the root lock against the thread's own replies (inboxDismissCheckAgentRoots,
-- api/inbox_dismiss.go), which is a comparison of reply ids and not of times. A time could
-- decide nothing here, for the reason written on 20261105093501's increment.
--
-- The root is found up in_reply_to, as user_threads_message (0095) finds an issue message's thread:
-- seven deep at most in production, and the walk stops at 64. A reply whose root is nobody's Agent
-- row deletes nothing, since no mark names it.
--
-- The root is taken `for key share` before the delete, which is what orders this trigger against
-- the dismiss route: that route takes the same root `for update` before it writes a mark
-- (inboxDismissLockAgentRoots, api/inbox_dismiss.go, step 2 of the lock order written on
-- markInboxReadThreads), and `for update` conflicts with `for key share`, so neither order loses
-- this reply. A dismiss that took the root first makes this transaction wait here, and the delete
-- below then sees the committed mark and removes it; a reply that holds the root first makes that
-- route wait, and its insert then runs on a snapshot holding this reply and writes no mark at all.
--
-- `for key share` rather than `for share`, which deadlocked: a reply written directly under the
-- root already holds `for key share` on it, taken by its own foreign key, so asking for the
-- stronger `for share` is an upgrade, and an upgrade behind a transaction already queued for
-- `for update` on that row - the accept and claim routes take the message row that way - is a
-- cycle Postgres breaks by killing one of them (TestAcceptAClaimAndAReplyOfOneAttemptAllLand saw
-- it as three 500s). Asking for the level the row is already held at waits for nothing, and a
-- reply deeper in the thread, which holds its own parent rather than the root, takes it afresh
-- without upgrading anything. Two sessions replying in one thread still do not queue behind each
-- other, since neither writes the row.
--
-- It is taken after the walk because the root is what the walk answers, and the walk reads rows
-- the delete never touches.
--
-- Its own migration, as each trigger of 0095-0097 is. `create function` takes no lock on any
-- table, and `create trigger ... after insert on messages` takes SHARE ROW EXCLUSIVE on messages:
-- it blocks that table's writers - every direct message and every issue message, for the moment the
-- catalogue row is written - and no reader, and it reads and rewrites no row. The census answers 0.
--
-- Measured on this pod's test database: user_agent_thread_dismissed held 0 rows and messages 25
-- after the api suite's Inbox seeds, the largest any one test's database held while
-- `go test ./internal/dispatch/api/ -run 'InboxRead|Replies|InboxRows'` ran. The window does not
-- grow with either count: attaching a trigger reads no row of either table.
create function user_agent_thread_dismissed_clear() returns trigger language plpgsql as $$
declare
  v_root uuid := new.id;
  v_parent uuid := new.in_reply_to;
  v_depth integer := 0;
begin
  -- A root is dismissed by the person who sent it; a root's own insert can clear nothing, and a
  -- write by anyone but the session leaves the mark where it is.
  if new.in_reply_to is null or new.author->>'kind' <> 'session' then
    return null;
  end if;
  while v_parent is not null and v_depth < 64 loop
    v_root := v_parent;
    select in_reply_to into v_parent from messages where id = v_root;
    v_depth := v_depth + 1;
  end loop;
  perform 1 from messages where id = v_root for key share;
  delete from user_agent_thread_dismissed where message_id = v_root;
  return null;
end;
$$;

create trigger user_agent_thread_dismissed_clear after insert on messages
  for each row execute function user_agent_thread_dismissed_clear();

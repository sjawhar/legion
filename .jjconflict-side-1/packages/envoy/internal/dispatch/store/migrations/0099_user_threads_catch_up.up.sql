-- 0099_user_threads_catch_up.up.sql
-- LEGION-687: catches up the writes 0098's seed could not see. A write by someone else (a comment,
-- an issue message, an answer) that commits while 0098 runs, on a thread whose participant 0098
-- is still inserting, reaches no row: the trigger's update cannot see the seed's uncommitted row,
-- and the seed's snapshot cannot see the write. That participant's row was seeded read with the
-- write missing, so its reply would never surface.
--
-- Every write the seed saw is no newer than the replied_at it gave the row, and every write since
-- 0098 committed moved the row through its trigger, so a write by someone else newer than a row's
-- replied_at is one neither counted. This moves the row's replied_at and last_reply_id to the
-- newest such write, which makes the row unread when that write is past its read mark; greatest()
-- keeps a row a trigger moved meanwhile where the trigger left it. A write stored before the
-- deploy stays read: the seed saw it and read the row through it ("Everything read",
-- dispatch://LEGION-687/spec). The writes are read as 0098 reads them (user_threads_login keys a
-- person; a message's thread is the root its in_reply_to walk reaches in at most 64 steps).
-- Reads comments, asks and messages (ACCESS SHARE) and updates only user_threads.
with recursive message_walk as (
  select id, id as ancestor, in_reply_to as parent, 0 as depth
  from messages where issue_key is not null
  union all
  select w.id, m.id, m.in_reply_to, w.depth + 1
  from message_walk w join messages m on m.id = w.parent
  where w.depth < 64
),
message_roots as (
  select distinct on (id) id, ancestor as root from message_walk order by id, depth desc
),
writes as (
  select coalesce(c.ask_id, c.reply_to, c.id) as thread_id, c.id as write_id,
         c.author->>'kind' as author_kind, c.author->>'id' as author_id, c.created_at as at
  from comments c
  union all
  select a.id, a.id, 'user', a.answer->>'user', (a.answer->>'at')::timestamptz
  from asks a where a.answer->>'user' is not null
  union all
  select r.root, m.id, m.author->>'kind', m.author->>'id', m.created_at
  from message_roots r join messages m on m.id = r.id
),
keyed as (
  select thread_id, write_id, at,
         case when author_kind = 'user' then user_threads_login(author_id) else author_kind || ':' || author_id end as writer
  from writes
),
missed as (
  select distinct on (u.login, u.thread_id) u.login, u.thread_id, k.at, k.write_id
  from user_threads u
  join keyed k on k.thread_id = u.thread_id and k.writer <> u.login
  where k.at > coalesce(u.replied_at, '-infinity')
  order by u.login, u.thread_id, k.at desc, k.write_id desc
)
update user_threads u set
  replied_at = greatest(u.replied_at, m.at),
  last_reply_id = case when u.replied_at is null or m.at > u.replied_at then m.write_id else u.last_reply_id end
from missed m
where u.login = m.login and u.thread_id = m.thread_id;

-- 0098_user_threads_seed.up.sql
-- LEGION-687: fills user_threads from history, one row per person per thread they wrote in: a
-- comment of theirs, an ask they asked, an answer they gave, or a message of theirs on an issue.
-- Every row starts read: the cutoff is the migration's own time, so the deploy turns no reply
-- already stored into an unread one, as 0051 did for direct messages (Sami's decision on
-- dispatch://LEGION-687/spec). replied_at and last_reply_id still record the newest reply anyone
-- else wrote, so a later reply is compared with it.
--
-- 0095-0097's triggers are live by now, so a write that lands while this runs already has its row;
-- the upsert merges with it by taking the later of each time, and never undoes the trigger's write.
-- Reads comments, asks, messages, issues and artifacts (ACCESS SHARE) and writes only user_threads.
insert into user_threads (login, thread_id, kind, project_key, issue_key, artifact_id, read_through, replied_at, last_reply_id)
with recursive cutoff(since) as (
  select now()
),
message_roots as (
  select id, id as root from messages where issue_key is not null and in_reply_to is null
  union all
  select m.id, r.root from messages m join message_roots r on m.in_reply_to = r.id
),
writes as (
  select coalesce(c.ask_id, c.reply_to, c.id) as thread_id,
         case when c.ask_id is not null then 'ask' else 'comment' end as kind,
         c.id as write_id, c.author->>'kind' as author_kind, c.author->>'id' as author_id,
         c.created_at as at, true as is_reply, c.issue_key, c.artifact_id
  from comments c
  union all
  select a.id, 'ask', null, 'user', a.author->>'id', a.created_at, false, a.issue_key, a.artifact_id
  from asks a where a.author->>'kind' = 'user'
  union all
  select a.id, 'ask', null, 'user', a.answer->>'user', (a.answer->>'at')::timestamptz, false, a.issue_key, a.artifact_id
  from asks a where a.answer->>'user' is not null
  union all
  select r.root, 'message', m.id, m.author->>'kind', m.author->>'id', m.created_at, true, m.issue_key, null
  from message_roots r join messages m on m.id = r.id
),
keyed as (
  select w.*,
         case when w.author_kind = 'user' then lower(w.author_id) else w.author_kind || ':' || w.author_id end as writer
  from writes w
),
mine as (
  select writer as login, thread_id, max(kind) as kind, max(at) as own_at,
         any_value(issue_key) as issue_key, any_value(artifact_id) as artifact_id
  from keyed where author_kind = 'user'
  group by writer, thread_id
),
newest_by_writer as (
  select distinct on (thread_id, writer) thread_id, writer, at, write_id
  from keyed where is_reply
  order by thread_id, writer, at desc, write_id desc
),
newest_other as (
  select distinct on (m.login, m.thread_id) m.login, m.thread_id, n.at, n.write_id
  from mine m join newest_by_writer n on n.thread_id = m.thread_id and n.writer <> m.login
  order by m.login, m.thread_id, n.at desc, n.write_id desc
)
select m.login, m.thread_id, m.kind, coalesce(i.project_key, ar.project_key), m.issue_key,
       case when m.issue_key is null then m.artifact_id end,
       case when o.at is not null and o.at <= c.since then greatest(m.own_at, o.at) else m.own_at end,
       o.at, o.write_id
from mine m
cross join cutoff c
left join issues i on i.key = m.issue_key
left join artifacts ar on m.issue_key is null and ar.id = m.artifact_id
left join newest_other o on o.login = m.login and o.thread_id = m.thread_id
where coalesce(i.project_key, ar.project_key) is not null
on conflict (login, thread_id) do update set
  read_through = greatest(user_threads.read_through, excluded.read_through),
  replied_at = greatest(user_threads.replied_at, excluded.replied_at),
  last_reply_id = case
    when user_threads.replied_at is null or excluded.replied_at > user_threads.replied_at then excluded.last_reply_id
    else user_threads.last_reply_id
  end;

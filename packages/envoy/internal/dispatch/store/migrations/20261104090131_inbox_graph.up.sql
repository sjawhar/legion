-- Current inbox membership is private to the own-inbox query and its comparison,
-- not part of refs.AllEdgesSQL: reassignment and a newer reply change these rows,
-- so they cannot describe historical delivery through the references API.
create view graph_inbox_edges (from_kind, from_id, kind, to_kind, to_id, created_at, source_seq,
                              cause, thread_id, session_id) as
  select 'ask', a.id::text, 'delivered_to', 'person', i.assignee, a.created_at, null::bigint,
         'ask', a.id::text, null::text
  from issues i join asks a on a.issue_key = i.key
  where i.assignee is not null and i.closed_at is null and a.state = 'open'
  union all
  select case when u.kind = 'message' then 'message'
              when u.kind = 'ask' and u.last_reply_id = u.thread_id then 'ask'
              else 'comment' end,
         u.last_reply_id::text, 'delivered_to', 'person', u.login, u.replied_at, null,
         'thread_reply', u.thread_id::text, null
  from user_threads u
  left join asks a on u.kind = 'ask' and a.id = u.thread_id
  left join issues ai on u.kind = 'ask' and ai.key = u.issue_key
  where u.last_reply_id is not null
    and not coalesce(a.state = 'open' and (u.issue_key is null or ai.closed_at is null), false)
  union all
  select 'message', reply.id::text, 'delivered_to', 'person', user_threads_login(root.author->>'id'), reply.created_at, null,
         'direct_reply', root.id::text, substr(root.target, length('session:') + 1)
  from messages root
  cross join lateral (
    with recursive chain as (
      select m.id, m.author, m.created_at, 1 as depth, array[root.id, m.id] as path
      from messages m where m.in_reply_to = root.id and m.id <> root.id
      union all
      select m.id, m.author, m.created_at, c.depth + 1, c.path || m.id
      from chain c join messages m on m.in_reply_to = c.id
      where c.depth < 64 and not m.id = any(c.path)
    )
    select id, created_at from chain where author->>'kind' = 'session'
  ) reply
  where root.issue_key is null and root.in_reply_to is null and root.target like 'session:%'
    and root.author->>'kind' = 'user';

create index messages_direct_roots_person on messages (user_threads_login(author->>'id'), target)
  where issue_key is null and in_reply_to is null and target like 'session:%'
    and author->>'kind' = 'user';

-- Observation counts alone are persisted. No inbox item or relation is copied here.
-- One transaction holding this singleton serializes scheduled passes.
create table inbox_shadow_control (id boolean primary key default true check (id));
insert into inbox_shadow_control default values;
create table inbox_shadow_days (
  day date not null,
  person text not null,
  first_checked timestamptz not null,
  last_checked timestamptz not null,
  samples bigint not null default 1,
  max_gap_seconds double precision not null default 0,
  mismatched bigint not null default 0,
  errors bigint not null default 0,
  missing jsonb not null default '{}',
  unexpected jsonb not null default '{}',
  expansions jsonb not null default '{}',
  primary key (day, person)
);
create table inbox_shadow_latest (
  person text primary key,
  checked_at timestamptz not null,
  horizon bigint not null,
  checked_items bigint not null,
  mismatched bigint not null,
  errors bigint not null,
  missing jsonb not null,
  unexpected jsonb not null,
  expansions jsonb not null
);

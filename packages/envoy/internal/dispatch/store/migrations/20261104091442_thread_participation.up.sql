-- Membership is derived from authorship's existing user_threads projection and
-- recorded mention/delivery relations. No new indexer or stored edge rows.
create view graph_thread_participation (person, thread_kind, thread_id, cause) as
  with sources as (
    select g.to_id as person, g.from_kind, g.from_id, 'mentioned'::text as cause
    from graph_mention_edges g where g.kind = 'mentions' and g.to_kind = 'person'
    union all
    -- Ask questions and current option/answer parts keep their identity links in
    -- refs/ask_block_refs; they are not document, comment, or message mentions.
    select r.to_id, 'ask', r.from_id, 'mentioned'
    from refs r where r.from_kind = 'ask' and r.kind = 'mentions' and r.to_kind = 'person_mention'
    union all
    select a.person, 'ask', r.from_id, 'mentioned'
    from refs r join person_aliases a on a.alias_kind = 'session' and a.alias = r.to_id
    where r.from_kind = 'ask' and r.kind = 'mentions' and r.to_kind = 'session_mention'
    union all
    select r.to_id, 'ask', r.ask_id::text, 'mentioned'
    from ask_block_refs r join ask_blocks b on b.ask_id = r.ask_id and b.block_id = r.block_id
    where b.position is not null and r.kind = 'mentions' and r.to_kind = 'person_mention'
    union all
    select a.person, 'ask', r.ask_id::text, 'mentioned'
    from ask_block_refs r join ask_blocks b on b.ask_id = r.ask_id and b.block_id = r.block_id
    join person_aliases a on a.alias_kind = 'session' and a.alias = r.to_id
    where b.position is not null and r.kind = 'mentions' and r.to_kind = 'session_mention'
    union all
    select g.to_id, g.from_kind, g.from_id, 'addressed'
    from graph_inbox_edges g where g.kind = 'delivered_to' and g.to_kind = 'person'
    union all
    select a.person, g.from_kind, g.from_id, 'addressed'
    from graph_person_edges g join person_aliases a on a.alias_kind = 'session' and a.alias = g.to_id
    where g.kind = 'delivered_to' and g.to_kind = 'session'
    union all
    -- user_threads deliberately excludes direct conversations. Their authorship
    -- remains derived from the same authored_by relation, not inferred ownership.
    select g.to_id, g.from_kind, g.from_id, 'authored'
    from graph_person_edges g join messages m on m.id::text = g.from_id and m.issue_key is null
    where g.kind = 'authored_by' and g.to_kind = 'person' and g.from_kind = 'message'
  ), source_nodes as (
    select s.person, s.cause, array_agg(s.from_kind) as kinds, array_agg(source.id) as ids
    from (select distinct person, cause, from_kind, from_id from sources) s
    cross join lateral (
      select id from comments where s.from_kind = 'comment' and id::text = s.from_id
      union all select id from messages where s.from_kind = 'message' and id::text = s.from_id
      union all select id from asks where s.from_kind = 'ask' and id::text = s.from_id
    ) source
    group by s.person, s.cause
  ), rooted as (
    select s.person, r.kind, r.id, s.cause
    from source_nodes s
    cross join lateral dispatch_thread_ancestors(s.kinds, s.ids) r
    left join artifacts a on a.id = r.artifact_id
    left join messages m on r.kind = 'message' and m.id = r.id
    where r.parent_id is null
      and (r.issue_key is not null or a.project_key is not null or m.target like 'session:%')
  )
  select login, kind, thread_id, 'authored' from user_threads
  union
  select person, kind, id, cause from rooted
  union
  -- The original user_threads seed saw only each ask's current answer. Recorded
  -- earlier answers still grant membership, without rewriting that projection.
  select user_threads_login(e.payload->'answer'->>'user'), 'ask', a.id, 'authored'
  from events e join asks a on a.id::text = e.payload->>'id'
  where e.type = 'ask.answered' and coalesce(user_threads_login(e.payload->'answer'->>'user'), '') <> '';

-- A sibling relation preserves old builds' graph_edges rows and query plans.
create view graph_thread_edges (from_kind, from_id, kind, to_kind, to_id, created_at, source_seq) as
  select 'person', p.person, 'participates_in', p.thread_kind, p.thread_id::text,
         coalesce(a.created_at,c.created_at,m.created_at), null::bigint
  from (select distinct person,thread_kind,thread_id from graph_thread_participation) p
  left join asks a on p.thread_kind = 'ask' and a.id = p.thread_id
  left join comments c on p.thread_kind = 'comment' and c.id = p.thread_id
  left join messages m on p.thread_kind = 'message' and m.id = p.thread_id;

create index events_participation_answer_person
  on events (user_threads_login(payload->'answer'->>'user'), (payload->>'id'))
  where type = 'ask.answered';

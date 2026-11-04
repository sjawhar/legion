-- Identity and delivery remain derived from the records their existing writers own.
-- A session's owner is set by bearerSessionActor from a verified personal or broker token.
-- Routes, claims and role holders alone prove no relationship between a role and a person.
create view person_aliases (person, alias_kind, alias) as
  with actors as not materialized (
    select author as actor from comments
    union all select author from messages
    union all select author from asks
    union all select actor from events
    union all select created_by from issues
    union all select claimed_by from issues
    union all select created_by from artifacts
    union all select credit.actor from artifact_versions v
      cross join lateral jsonb_array_elements(nullif(v.authors, 'null'::jsonb)) credit(actor)
  ), aliases as (
    select user_threads_login(email) as person, 'login' as alias_kind, user_threads_login(email) as alias from people
    union all
    select user_threads_login(actor->>'id'), 'login', user_threads_login(actor->>'id')
      from actors where actor->>'kind' = 'user'
    union all
    select user_threads_login(actor->>'id'), 'actor_id', actor->>'id'
      from actors where actor->>'kind' = 'user'
    union all
    select user_threads_login(actor->>'owner'), 'login', user_threads_login(actor->>'owner')
      from actors where actor->>'kind' = 'session'
    union all
    select user_threads_login(actor->>'owner'), 'session', actor->>'id'
      from actors where actor->>'kind' = 'session'
  )
  select distinct person, alias_kind, alias from aliases
  where person <> '' and alias <> '';

-- The graph is graph_edges plus graph_person_edges (refs.AllEdgesSQL). Keeping these
-- derived arms separate leaves old builds' reads and plans unchanged during a roll or rollback.
-- New readers use the shared union; neither view has a second writer or a stored edge row.
create view graph_person_edges (from_kind, from_id, kind, to_kind, to_id, created_at, source_seq) as
  select 'comment', id::text, 'authored_by', 'person', user_threads_login(author->>'id'), created_at, null::bigint
  from comments where author->>'kind' = 'user' and user_threads_login(author->>'id') <> ''
  union all
  select 'comment', id::text, 'authored_by', 'session', author->>'id', created_at, null
  from comments where author->>'kind' = 'session' and author->>'id' <> ''
  union all
  select 'message', id::text, 'authored_by', 'person', user_threads_login(author->>'id'), created_at, null
  from messages where author->>'kind' = 'user' and user_threads_login(author->>'id') <> ''
  union all
  select 'message', id::text, 'authored_by', 'session', author->>'id', created_at, null
  from messages where author->>'kind' = 'session' and author->>'id' <> ''
  union all
  select 'ask', id::text, 'authored_by', 'person', user_threads_login(author->>'id'), created_at, null
  from asks where author->>'kind' = 'user' and user_threads_login(author->>'id') <> ''
  union all
  select 'ask', id::text, 'authored_by', 'session', author->>'id', created_at, null
  from asks where author->>'kind' = 'session' and author->>'id' <> ''
  union all
  select 'comment', e.payload->>'id', 'delivered_to', d.kind,
         substring(t.topic from length(d.prefix) + 1), e.created_at, e.id
  from events e cross join lateral unnest(e.published_destinations) t(topic)
  cross join (values ('session', 'notifications.agent.'), ('role', 'notifications.role.')) d(kind, prefix)
  where e.type = 'comment.created' and e.payload->>'id' <> ''
    and starts_with(t.topic, d.prefix) and length(t.topic) > length(d.prefix)
  union all
  select 'message', e.payload->>'id', 'delivered_to', d.kind,
         substring(t.topic from length(d.prefix) + 1), e.created_at, e.id
  from events e cross join lateral unnest(e.published_destinations) t(topic)
  cross join (values ('session', 'notifications.agent.'), ('role', 'notifications.role.')) d(kind, prefix)
  where e.type in ('message.created', 'message.answered') and e.payload->>'id' <> ''
    and starts_with(t.topic, d.prefix) and length(t.topic) > length(d.prefix)
  union all
  select 'ask', e.payload->>'id', 'delivered_to', d.kind,
         substring(t.topic from length(d.prefix) + 1), e.created_at, e.id
  from events e cross join lateral unnest(e.published_destinations) t(topic)
  cross join (values ('session', 'notifications.agent.'), ('role', 'notifications.role.')) d(kind, prefix)
  where e.type = 'ask.answered' and e.payload->>'id' <> ''
    and starts_with(t.topic, d.prefix) and length(t.topic) > length(d.prefix)
  union all
  select 'message', message_id::text, 'delivered_to', 'session', session_id, created_at, null
  from message_deliveries where state = 'sent' and session_id <> ''
  union all
  select 'comment', comment_id::text, 'delivered_to', 'session', session_id, created_at, null
  from comment_deliveries where state = 'sent' and session_id <> '';

-- Identity and delivery remain derived from the records their existing writers own.
-- A broker-issued credential proves its session; every other bearer names one in its body.
-- Only actors carrying the server-written session_verified flag establish session aliases.
-- Routes, claims and role holders alone prove no relationship between a role and a person.
create function person_graph_login(actor jsonb) returns text language sql immutable
  return user_threads_login(case actor->>'kind'
    when 'user' then actor->>'id' when 'session' then actor->>'owner' end);

-- These expressions are also the alias view's predicates, so a person's lookup reaches only
-- that person's records instead of canonicalising every actor in the event log.
create index comments_person_login on comments (person_graph_login(author));
create index messages_person_login on messages (person_graph_login(author));
create index asks_person_login on asks (person_graph_login(author));
create index events_person_login on events (person_graph_login(actor));
create index issues_creator_person_login on issues (person_graph_login(created_by));
create index issues_claimant_person_login on issues (person_graph_login(claimed_by));
create index artifacts_creator_person_login on artifacts (person_graph_login(created_by));
create index people_person_login on people (user_threads_login(email));

-- Published destinations are the recorded recipient set. The named person read matches whole
-- topics in this partial index, then bounds events by created_at; it never matches substrings.
create index events_graph_delivery_topics on events using gin (published_destinations)
  where type in ('ask.answered', 'comment.created', 'message.created', 'message.answered')
    and payload->>'id' <> '';
create index events_graph_delivery_created on events (created_at desc)
  where type in ('ask.answered', 'comment.created', 'message.created', 'message.answered')
    and payload->>'id' <> '';

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
    select person_graph_login(actor), 'login', person_graph_login(actor)
      from actors where actor->>'kind' in ('user', 'session')
    union all
    select person_graph_login(actor), 'actor_id', actor->>'id'
      from actors where actor->>'kind' = 'user'
    union all
    select person_graph_login(actor), 'session', actor->>'id'
      from actors where actor->>'kind' = 'session' and actor->>'session_verified' = 'true'
  )
  select distinct person, alias_kind, alias from aliases
  where person <> '' and alias <> '';

-- The graph is graph_edges plus graph_person_edges (refs.AllEdgesSQL). Keeping these
-- derived arms separate leaves old builds' reads and plans unchanged during a roll or rollback.
-- New readers use the shared union; neither view has a second writer or a stored edge row.
create view graph_person_edges (from_kind, from_id, kind, to_kind, to_id, created_at, source_seq) as
  select 'comment', id::text, 'authored_by', 'person', person_graph_login(author), created_at, null::bigint
  from comments where author->>'kind' = 'user' and person_graph_login(author) <> ''
  union all
  select 'comment', id::text, 'authored_by', 'session', author->>'id', created_at, null
  from comments where author->>'kind' = 'session' and author->>'id' <> ''
  union all
  select 'message', id::text, 'authored_by', 'person', person_graph_login(author), created_at, null
  from messages where author->>'kind' = 'user' and person_graph_login(author) <> ''
  union all
  select 'message', id::text, 'authored_by', 'session', author->>'id', created_at, null
  from messages where author->>'kind' = 'session' and author->>'id' <> ''
  union all
  select 'ask', id::text, 'authored_by', 'person', person_graph_login(author), created_at, null
  from asks where author->>'kind' = 'user' and person_graph_login(author) <> ''
  union all
  select 'ask', id::text, 'authored_by', 'session', author->>'id', created_at, null
  from asks where author->>'kind' = 'session' and author->>'id' <> ''
  union all
  select 'comment', e.payload->>'id', 'delivered_to', 'session',
         substring(t.topic from 21), e.created_at, e.id
  from events e cross join lateral unnest(e.published_destinations) t(topic)
  where e.type = 'comment.created' and e.payload->>'id' <> ''
    and starts_with(t.topic, 'notifications.agent.') and length(t.topic) > 20
  union all
  select 'comment', e.payload->>'id', 'delivered_to', 'role',
         substring(t.topic from 20), e.created_at, e.id
  from events e cross join lateral unnest(e.published_destinations) t(topic)
  where e.type = 'comment.created' and e.payload->>'id' <> ''
    and starts_with(t.topic, 'notifications.role.') and length(t.topic) > 19
  union all
  select 'message', e.payload->>'id', 'delivered_to', 'session',
         substring(t.topic from 21), e.created_at, e.id
  from events e cross join lateral unnest(e.published_destinations) t(topic)
  where e.type in ('message.created', 'message.answered') and e.payload->>'id' <> ''
    and starts_with(t.topic, 'notifications.agent.') and length(t.topic) > 20
  union all
  select 'message', e.payload->>'id', 'delivered_to', 'role',
         substring(t.topic from 20), e.created_at, e.id
  from events e cross join lateral unnest(e.published_destinations) t(topic)
  where e.type in ('message.created', 'message.answered') and e.payload->>'id' <> ''
    and starts_with(t.topic, 'notifications.role.') and length(t.topic) > 19
  union all
  select 'ask', e.payload->>'id', 'delivered_to', 'session',
         substring(t.topic from 21), e.created_at, e.id
  from events e cross join lateral unnest(e.published_destinations) t(topic)
  where e.type = 'ask.answered' and e.payload->>'id' <> ''
    and starts_with(t.topic, 'notifications.agent.') and length(t.topic) > 20
  union all
  select 'ask', e.payload->>'id', 'delivered_to', 'role',
         substring(t.topic from 20), e.created_at, e.id
  from events e cross join lateral unnest(e.published_destinations) t(topic)
  where e.type = 'ask.answered' and e.payload->>'id' <> ''
    and starts_with(t.topic, 'notifications.role.') and length(t.topic) > 19
  union all
  select 'message', message_id::text, 'delivered_to', 'session', session_id, created_at, null
  from message_deliveries where state = 'sent' and session_id <> ''
  union all
  select 'comment', comment_id::text, 'delivered_to', 'session', session_id, created_at, null
  from comment_deliveries where state = 'sent' and session_id <> '';

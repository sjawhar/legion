-- Identity mentions use unknown storage kinds in the existing reference index, so old
-- builds skip them. This sibling maps them without changing graph_edges or any existing row.
create view graph_mention_resolutions (from_kind, from_id, to_kind, to_id, created_at, source_seq) as
    select 'comment'::text as from_kind, r.from_id, target.kind as to_kind, r.to_id, c.created_at, r.source_seq
    from refs r join comments c on c.id::text = r.from_id
    join (values ('person_mention', 'person'), ('session_mention', 'session'), ('role_mention', 'role')) target(storage, kind)
      on r.to_kind = target.storage
    where r.from_kind = 'comment'
    union all
    select 'message', r.from_id, target.kind, r.to_id, m.created_at, r.source_seq
    from refs r join messages m on m.id::text = r.from_id
    join (values ('person_mention', 'person'), ('session_mention', 'session'), ('role_mention', 'role')) target(storage, kind)
      on r.to_kind = target.storage
    where r.from_kind = 'message'
    union all
    select 'block', r.from_node, target.kind, r.to_id, d.changed_at, r.source_seq
    from block_refs r join document_blocks d on d.node_id = r.from_node
    join (values ('person_mention', 'person'), ('session_mention', 'session'), ('role_mention', 'role')) target(storage, kind)
      on r.to_kind = target.storage
    where r.kind = 'mentions'
    union all
    select 'comment', m.comment_id::text, target.kind, substring(m.target from length(target.prefix) + 1), c.created_at, null
    from comment_mentions m join comments c on c.id = m.comment_id
    cross join (values ('session', 'session:'), ('role', 'role:')) target(kind, prefix)
    where starts_with(m.target, target.prefix)
    union all
    select 'comment', m.comment_id::text, 'session', m.resolved_session_id, c.created_at, null
    from comment_mentions m join comments c on c.id = m.comment_id
    where m.resolved_session_id <> '' and m.target <> 'session:' || m.resolved_session_id
    union all
    select 'comment', d.comment_id::text, 'session', d.session_id, c.created_at, null
    from comment_deliveries d join comments c on c.id = d.comment_id
    where d.session_id <> '' and (d.resolve_error is null or d.resolve_error not like 'no live session %')
    union all
    select 'message', m.id::text, target.kind, substring(m.target from length(target.prefix) + 1), m.created_at, null
    from messages m cross join (values ('session', 'session:'), ('role', 'role:')) target(kind, prefix)
    where starts_with(m.target, target.prefix)
    union all
    select 'message', d.message_id::text, 'session', d.session_id, m.created_at, null
    from message_deliveries d join messages m on m.id = d.message_id where d.session_id <> ''
;

create view graph_mention_edges (from_kind, from_id, kind, to_kind, to_id, created_at, source_seq) as
  select from_kind, from_id, 'mentions', to_kind, to_id, min(created_at), min(source_seq)
  from (
    select * from graph_mention_resolutions
    union all
    select r.from_kind, r.from_id, 'person', a.person, r.created_at, r.source_seq
    from graph_mention_resolutions r join person_aliases a on a.alias_kind = 'session' and a.alias = r.to_id
    where r.to_kind = 'session'
  ) expanded
  where to_id <> '' and to_kind in ('person', 'session', 'role')
  group by from_kind, from_id, to_kind, to_id;

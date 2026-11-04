-- Identity mentions use unknown storage kinds in the existing reference index, so old
-- builds skip them. This sibling maps them without changing graph_edges or any existing row.
create view graph_mention_edges (from_kind, from_id, kind, to_kind, to_id, created_at, source_seq) as
  with recorded as not materialized (
    select r.from_kind, r.from_id, left(r.to_kind, -8) as to_kind, r.to_id, r.created_at, r.source_seq
    from refs r where r.to_kind in ('person_mention', 'session_mention', 'role_mention')
      and r.from_kind in ('comment', 'message')
    union all
    select 'block', from_node, left(to_kind, -8), to_id, created_at, source_seq
    from block_refs where to_kind in ('person_mention', 'session_mention', 'role_mention') and kind = 'mentions'
    union all
    select 'comment', m.comment_id::text, split_part(m.target, ':', 1), substring(m.target from position(':' in m.target) + 1), c.created_at, null
    from comment_mentions m join comments c on c.id = m.comment_id
    union all
    select 'comment', m.comment_id::text, 'session', m.resolved_session_id, c.created_at, null
    from comment_mentions m join comments c on c.id = m.comment_id where m.resolved_session_id <> ''
    union all
    select 'comment', d.comment_id::text, 'session', d.session_id, d.created_at, null
    from comment_deliveries d where d.session_id <> '' and (d.resolve_error is null or d.resolve_error not like 'no live session %')
    union all
    select 'message', m.id::text, split_part(m.target, ':', 1), substring(m.target from position(':' in m.target) + 1), m.created_at, null
    from messages m where m.target like 'session:%' or m.target like 'role:%'
    union all
    select 'message', d.message_id::text, 'session', d.session_id, d.created_at, null
    from message_deliveries d where d.session_id <> ''
  ), expanded as (
    select * from recorded
    union all
    select r.from_kind, r.from_id, 'person', a.person, r.created_at, r.source_seq
    from recorded r join person_aliases a on a.alias_kind = 'session' and a.alias = r.to_id
    where r.to_kind = 'session'
  )
  select from_kind, from_id, 'mentions', to_kind, to_id, min(created_at), min(source_seq)
  from expanded where to_id <> '' and to_kind in ('person', 'session', 'role')
  group by from_kind, from_id, to_kind, to_id;

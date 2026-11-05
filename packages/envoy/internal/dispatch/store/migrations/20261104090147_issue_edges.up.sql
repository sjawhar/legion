-- One source of truth for explicit filing provenance: only issue creation writes this
-- nullable reference. Old builds omit it and keep working. No row backfill or graph_edges DDL.
alter table issues add column filed_from text;

create view graph_issue_edges (from_kind, from_id, kind, to_kind, to_id, created_at, source_seq) as
  select 'issue', i.key, 'duplicate_of', 'issue', target.key, note.created_at, null::bigint
  from issues i
  cross join lateral (
    select max(changed.created_at) as reopened_at
    from (
      select e.created_at, e.payload->>'status' as status,
             lag(e.payload->>'status') over (order by e.seq) as previous_status
      from events e
      where e.issue_key = i.key and e.type in ('issue.created', 'issue.updated', 'issue.closed')
    ) changed
    where changed.previous_status = 'done' and changed.status <> 'done'
  ) lifetime
  cross join lateral (
    select substring(n.body from '(?i)^ {0,3}Duplicate of\s+dispatch://([A-Z][A-Z0-9_-]*-[0-9]+)(?:[.[:space:]]|$)') as target,
           n.created_at
    from (
      select m.id, m.body, m.created_at from messages m
      where m.issue_key = i.key and m.created_at <= i.closed_at
      union all
      select c.id, c.body, coalesce(c.edited_at, c.created_at) as created_at from comments c
      where c.issue_key = i.key and coalesce(c.edited_at, c.created_at) <= i.closed_at
    ) n
    where n.body ~* '^ {0,3}Duplicate of\s+dispatch://[A-Z][A-Z0-9_-]*-[0-9]+(?:[.[:space:]]|$)'
      and n.created_at > coalesce(lifetime.reopened_at, '-infinity'::timestamptz)
    order by n.created_at desc, n.id desc limit 1
  ) note
  join issues target on target.key = note.target and target.key <> i.key
  where i.status = 'done'
  union all
  select 'issue', i.key, 'filed_from', parts[1], parts[2], i.created_at, null::bigint
  from issues i
  cross join lateral regexp_match(i.filed_from, '/(ask|comment|message)/([^/]+)$') origin(parts)
  where i.filed_from is not null;

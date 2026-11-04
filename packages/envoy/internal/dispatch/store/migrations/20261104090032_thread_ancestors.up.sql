-- One ancestor walk for thread writes and graph reads. Keep owner boundaries, a
-- visited-node guard, and the existing caps (32 comment nodes; 64 message links).
-- A broken/cyclic/over-depth chain has no terminal row, never an invented root.
-- NULL is not a node. STRICT also keeps this set-returning walk as one function
-- scan rather than inlining its recursive plan into every sibling graph arm.
create function dispatch_thread_ancestors(p_kind text, p_id uuid)
returns table (kind text, id uuid, parent_id uuid, depth integer, issue_key text, artifact_id uuid)
language sql stable strict rows 8 as $$
  with recursive walk as (
    select n.*, 0 as depth, array[n.kind || ':' || n.id::text] as visited
    from (
      select 'comment'::text as kind, c.id,
             case when c.ask_id is not null then 'ask' else 'comment' end as parent_kind,
             coalesce(c.ask_id, c.reply_to) as parent_id, c.issue_key, c.artifact_id
      from comments c where p_kind = 'comment' and c.id = p_id
      union all
      select 'message', m.id, 'message', m.in_reply_to, m.issue_key, null::uuid
      from messages m where p_kind = 'message' and m.id = p_id
      union all
      select 'ask', a.id, null, null::uuid, a.issue_key, a.artifact_id
      from asks a where p_kind = 'ask' and a.id = p_id
    ) n
    union all
    select n.*, w.depth + 1, w.visited || (n.kind || ':' || n.id::text)
    from walk w
    cross join lateral (
      select 'comment'::text as kind, c.id,
             case when c.ask_id is not null then 'ask' else 'comment' end as parent_kind,
             coalesce(c.ask_id, c.reply_to) as parent_id, c.issue_key, c.artifact_id
      from comments c where w.parent_kind = 'comment' and c.id = w.parent_id
      union all
      select 'message', m.id, 'message', m.in_reply_to, m.issue_key, null::uuid
      from messages m where w.parent_kind = 'message' and m.id = w.parent_id
      union all
      select 'ask', a.id, null, null::uuid, a.issue_key, a.artifact_id
      from asks a where w.parent_kind = 'ask' and a.id = w.parent_id
    ) n
    where (w.depth < case when p_kind = 'message' then 64 else 31 end
           or (p_kind = 'comment' and w.depth = 31 and w.parent_kind = 'ask'))
      and not (n.kind || ':' || n.id::text = any(w.visited))
      and n.issue_key is not distinct from w.issue_key
      and n.artifact_id is not distinct from w.artifact_id
  )
  select w.kind, w.id, w.parent_id, w.depth, w.issue_key, w.artifact_id from walk w
$$;

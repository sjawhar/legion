-- One ancestor walk for thread writes and graph reads. Keep owner boundaries and a
-- visited-node guard. A comment chain keeps the 32-comment cap the comment write path
-- already had (parentDepthCap); a message chain is walked to its root with no depth
-- limit, because the message routes this function replaced had none and a valid chain
-- of any length must still resolve. The visited array is the only thing that stops a
-- cyclic chain, and it stops it in one pass rather than holding a transaction open.
-- A broken/cyclic/over-depth chain has no terminal row, never an invented root.
-- Batch request-sized walks rather than planning one function execution per
-- source. Interpreting the indexed walk avoids JIT compilation dominating reads.
create function dispatch_thread_ancestors(p_kinds text[], p_ids uuid[])
returns table (kind text, id uuid, parent_id uuid, depth integer, issue_key text, artifact_id uuid)
set jit = off
language sql stable strict as $$
  with recursive walk as (
    select n.*, seed.kind as seed_kind, 0 as depth, array[n.kind || ':' || n.id::text] as visited
    from unnest(p_kinds, p_ids) seed(kind, id)
    cross join lateral (
      select 'comment'::text as kind, c.id,
             case when c.ask_id is not null then 'ask' else 'comment' end as parent_kind,
             coalesce(c.ask_id, c.reply_to) as parent_id, c.issue_key, c.artifact_id
      from comments c where seed.kind = 'comment' and c.id = seed.id
      union all
      select 'message', m.id, 'message', m.in_reply_to, m.issue_key, null::uuid
      from messages m where seed.kind = 'message' and m.id = seed.id
      union all
      select 'ask', a.id, null, null::uuid, a.issue_key, a.artifact_id
      from asks a where seed.kind = 'ask' and a.id = seed.id
    ) n
    union all
    select n.*, w.seed_kind, w.depth + 1, w.visited || (n.kind || ':' || n.id::text)
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
    where (w.seed_kind = 'message'
           or w.depth < 31
           or (w.seed_kind = 'comment' and w.depth = 31 and w.parent_kind = 'ask'))
      and not (n.kind || ':' || n.id::text = any(w.visited))
      and n.issue_key is not distinct from w.issue_key
      and n.artifact_id is not distinct from w.artifact_id
  )
  select w.kind, w.id, w.parent_id, w.depth, w.issue_key, w.artifact_id from walk w
$$;

-- Actual writes, not read marks, order My threads. Walk all candidate roots in
-- one execution, using the reply/ask indexes rather than one SQL function call
-- per candidate. Root lookup itself is dispatch_thread_ancestors alone.
-- The indexed work is cheaper than compiling a per-request recursive JIT plan.
create function dispatch_thread_activity(p_kinds text[], p_ids uuid[])
returns table (thread_kind text, thread_id uuid, activity_at timestamptz)
set jit = off
language sql stable strict as $$
  with recursive writes as (
    select root.kind as root_kind, root.id as root_id, n.*,
           array[n.kind || ':' || n.id::text] as visited
    from unnest(p_kinds, p_ids) root(kind, id)
    cross join lateral (
      select 'comment'::text as kind, id, created_at as at, issue_key, artifact_id from comments
      where root.kind = 'comment' and id = root.id
      union all
      select 'message', id, created_at, issue_key, null::uuid from messages
      where root.kind = 'message' and id = root.id
      union all
      select 'ask', a.id,
             greatest(a.created_at, (a.answer->>'at')::timestamptz,
               (select max((e.payload->'answer'->>'at')::timestamptz) from events e
                where e.type = 'ask.answered' and e.payload->>'id' = a.id::text)),
             a.issue_key, a.artifact_id from asks a
      where root.kind = 'ask' and a.id = root.id
    ) n
    union all
    select w.root_kind, w.root_id, n.*, w.visited || (n.kind || ':' || n.id::text)
    from writes w cross join lateral (
      select 'comment'::text as kind, c.id, c.created_at as at, c.issue_key, c.artifact_id
      from comments c where w.kind = 'comment' and c.reply_to = w.id
      union all
      select 'comment', c.id, c.created_at, c.issue_key, c.artifact_id
      from comments c where w.kind = 'ask' and c.ask_id = w.id
      union all
      select 'message', m.id, m.created_at, m.issue_key, null::uuid
      from messages m where w.kind = 'message' and m.in_reply_to = w.id
    ) n
    where not (n.kind || ':' || n.id::text = any(w.visited))
      and n.issue_key is not distinct from w.issue_key
      and n.artifact_id is not distinct from w.artifact_id
  ) select root_kind, root_id, max(at) from writes group by root_kind, root_id
$$;

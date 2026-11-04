-- Actual writes, not read marks, order My threads. Each downward activity walk
-- starts at a candidate root and uses the existing reply/ask indexes. This is
-- distinct from finding a root: ancestry is dispatch_thread_ancestors alone.
create function dispatch_thread_activity(p_kind text, p_id uuid) returns timestamptz
language sql stable strict as $$
  with recursive writes as (
    select n.*, 0 as depth, array[n.kind || ':' || n.id::text] as visited
    from (
      select 'comment'::text as kind, id, created_at as at, issue_key, artifact_id from comments
      where p_kind = 'comment' and id = p_id
      union all
      select 'message', id, created_at, issue_key, null::uuid from messages
      where p_kind = 'message' and id = p_id
      union all
      select 'ask', a.id,
             greatest(a.created_at, (a.answer->>'at')::timestamptz,
               (select max((e.payload->'answer'->>'at')::timestamptz) from events e
                where e.type = 'ask.answered' and e.payload->>'id' = a.id::text)),
             a.issue_key, a.artifact_id from asks a
      where p_kind = 'ask' and a.id = p_id
    ) n
    union all
    select n.*, w.depth + 1, w.visited || (n.kind || ':' || n.id::text)
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
    where w.depth < 64 and not (n.kind || ':' || n.id::text = any(w.visited))
      and n.issue_key is not distinct from w.issue_key
      and n.artifact_id is not distinct from w.artifact_id
  ) select max(at) from writes
$$;

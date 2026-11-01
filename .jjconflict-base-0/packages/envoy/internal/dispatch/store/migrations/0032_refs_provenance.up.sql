-- 0032_refs_provenance.up.sql
-- Every mention edge carries its provenance: the edge type (only 'mentions' is derived from
-- text today), the moment the (from, to) pair first appeared, and the events.id of the write
-- that introduced it. source_seq is the global bigserial events.id, not the per-owner
-- events.seq: it orders edges across issues and documents, and ?since= on the read API
-- compares against it. refs.ReplaceCounted reconciles instead of rewriting, so a surviving edge
-- keeps created_at and source_seq; the columns are stamped after the source's event is appended.
alter table refs
  add column kind text not null default 'mentions',
  add column created_at timestamptz not null default now(),
  add column source_seq bigint;

-- graph_edges casts anchor.artifact_id to uuid on every row of asks and comments; one legacy
-- non-uuid anchor would break every to=artifact query, so refuse the migration instead. Every
-- writer stores artifacts.id there (api/anchors.go resolveAnchor).
do $$
declare
  bad record;
begin
  select 'asks' as source, id, anchor->>'artifact_id' as artifact_id into bad
  from asks
  where anchor->>'artifact_id' is not null
    and anchor->>'artifact_id' !~* '^[0-9a-f]{8}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{12}$'
  limit 1;
  if found then
    raise exception '0032_refs_provenance: %.% anchor.artifact_id % is not a uuid', bad.source, bad.id, bad.artifact_id;
  end if;
  select 'comments' as source, id, anchor->>'artifact_id' as artifact_id into bad
  from comments
  where anchor->>'artifact_id' is not null
    and anchor->>'artifact_id' !~* '^[0-9a-f]{8}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{12}$'
  limit 1;
  if found then
    raise exception '0032_refs_provenance: %.% anchor.artifact_id % is not a uuid', bad.source, bad.id, bad.artifact_id;
  end if;
end $$;

-- Each structural arm of graph_edges filters on one of these; the expression indexes match the
-- arm's select list so a to_id or from_id predicate pushed into the arm hits an index.
create index issues_parent_key on issues (parent_key) where parent_key is not null;
create index artifacts_issue_key on artifacts (issue_key) where issue_key is not null;
create index artifacts_id_text on artifacts ((id::text));
create index asks_id_text on asks ((id::text));
create index asks_anchor_artifact on asks ((coalesce(block_artifact_id, (anchor->>'artifact_id')::uuid)));
create index asks_artifact_id on asks (artifact_id) where artifact_id is not null;
create index comments_id_text on comments ((id::text));
create index comments_anchor_artifact on comments (((anchor->>'artifact_id')::uuid));
create index comments_artifact_id on comments (artifact_id) where artifact_id is not null;
create index comments_reply_to_text on comments ((reply_to::text));
create index comments_ask_id_text on comments ((ask_id::text));
create index messages_id_text on messages ((id::text));
create index messages_in_reply_to_text on messages ((in_reply_to::text));
create index ask_followers_ask_id_text on ask_followers ((ask_id::text));

-- The one edge relation. Mentions come from refs; every structural relation is read from the
-- column that owns it, typed by kind. Artifact targets are ref_key (`<IssueKey|Project>/<slug>`),
-- the address refs stores; artifact sources are the artifact uuid, the address the reconcile
-- writes on the from side. Structural edges carry no source_seq.
create view graph_edges (from_kind, from_id, kind, to_kind, to_id, created_at, source_seq) as
  select from_kind, from_id, kind, to_kind, to_id, created_at, source_seq from refs
  union all
  select 'issue', key, 'child_of', 'issue', parent_key, created_at, null
  from issues where parent_key is not null
  union all
  select 'artifact', id::text, 'attached_to', 'issue', issue_key, created_at, null
  from artifacts where issue_key is not null
  union all
  select 'ask', k.id::text, 'anchored_to', 'artifact', a.ref_key, k.created_at, null
  from asks k join artifacts a on a.id = coalesce(k.block_artifact_id, (k.anchor->>'artifact_id')::uuid)
  union all
  select 'comment', c.id::text, 'anchored_to', 'artifact', a.ref_key, c.created_at, null
  from comments c join artifacts a on a.id = (c.anchor->>'artifact_id')::uuid
  union all
  select 'ask', k.id::text, 'owned_by', 'artifact', a.ref_key, k.created_at, null
  from asks k join artifacts a on a.id = k.artifact_id
  union all
  select 'comment', c.id::text, 'owned_by', 'artifact', a.ref_key, c.created_at, null
  from comments c join artifacts a on a.id = c.artifact_id
  union all
  select 'comment', id::text, 'replies_to', 'comment', reply_to::text, created_at, null
  from comments where reply_to is not null
  union all
  select 'comment', id::text, 'replies_to', 'ask', ask_id::text, created_at, null
  from comments where ask_id is not null
  union all
  select 'message', id::text, 'replies_to', 'message', in_reply_to::text, created_at, null
  from messages where in_reply_to is not null
  union all
  select 'ask', ask_id::text, 'followed_by', 'session', session_id, since, null
  from ask_followers;

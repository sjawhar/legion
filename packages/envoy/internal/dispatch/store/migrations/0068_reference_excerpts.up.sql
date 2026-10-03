-- 0068_reference_excerpts.up.sql
--
-- The reference graph records a document mention's first containing block when that document is
-- indexed. Backlink reads can then use the stored block id and markdown excerpt without loading
-- the citing document. `excerpt_ready` distinguishes a populated document edge from pre-cutover
-- data; an edge that has no containing block remains ready with empty text and uses the existing
-- source-name fallback.
alter table refs
  add column excerpt_block_id text not null default '',
  add column excerpt_text text not null default '',
  add column excerpt_ready boolean not null default false;

create index refs_document_excerpt_pending
  on refs (from_id)
  where from_kind = 'artifact' and not excerpt_ready;

-- graph_edges re-stated from 0038 with the stored document excerpt fields. Structural relations
-- have no source body, so they preserve their existing excerpt resolution with empty metadata.
create or replace view graph_edges (
  from_kind,
  from_id,
  kind,
  to_kind,
  to_id,
  created_at,
  source_seq,
  excerpt_block_id,
  excerpt_text,
  excerpt_ready
) as
  select from_kind, from_id, kind, to_kind, to_id, created_at, source_seq,
         excerpt_block_id, excerpt_text, excerpt_ready
  from refs
  union all
  select 'issue', key, 'child_of', 'issue', parent_key, created_at, null, '', '', false
  from issues where parent_key is not null
  union all
  select 'artifact', id::text, 'attached_to', 'issue', issue_key, created_at, null, '', '', false
  from artifacts where issue_key is not null
  union all
  select 'ask', k.id::text, 'anchored_to', 'artifact', a.ref_key, k.created_at, null, '', '', false
  from asks k join artifacts a on a.id = coalesce(k.block_artifact_id, (k.anchor->>'artifact_id')::uuid)
  union all
  select 'comment', c.id::text, 'anchored_to', 'artifact', a.ref_key, c.created_at, null, '', '', false
  from comments c join artifacts a on a.id = (c.anchor->>'artifact_id')::uuid
  union all
  select 'ask', k.id::text, 'owned_by', 'artifact', a.ref_key, k.created_at, null, '', '', false
  from asks k join artifacts a on a.id = k.artifact_id
  union all
  select 'comment', c.id::text, 'owned_by', 'artifact', a.ref_key, c.created_at, null, '', '', false
  from comments c join artifacts a on a.id = c.artifact_id
  union all
  select 'comment', id::text, 'replies_to', 'comment', reply_to::text, created_at, null, '', '', false
  from comments where reply_to is not null
  union all
  select 'comment', id::text, 'replies_to', 'ask', ask_id::text, created_at, null, '', '', false
  from comments where ask_id is not null
  union all
  select 'message', id::text, 'replies_to', 'message', in_reply_to::text, created_at, null, '', '', false
  from messages where in_reply_to is not null
  union all
  select 'ask', ask_id::text, 'followed_by', 'session', session_id, since, null, '', '', false
  from ask_followers
  union all
  select 'component', c.project_key || '/' || c.id, 'part_of', 'component', c.project_key || '/' || c.parent,
         s.imported_at, null, '', '', false
  from components c join architecture_snapshots s on s.id = c.snapshot_id
  where c.parent is not null
  union all
  select 'component', d.project_key || '/' || d.from_id, 'depends_on', 'component', d.project_key || '/' || d.to_id,
         s.imported_at, null, '', '', false
  from component_depends d
  join components c on c.project_key = d.project_key and c.id = d.from_id
  join architecture_snapshots s on s.id = c.snapshot_id
  union all
  select 'issue', m.issue_key, 'affects', 'component', m.project_key || '/' || m.component_id,
         c.updated_at, null, '', '', false
  from issue_component_members m join issue_components c using (issue_key);

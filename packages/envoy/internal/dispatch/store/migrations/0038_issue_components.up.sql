-- 0038_issue_components.up.sql
--
-- An issue's own component attachment. No row means the issue inherits its
-- nearest ancestor's attachment (the default). A row is either an explicit set
-- (its members live in issue_component_members) or `none` with the reason the
-- issue is not architectural; either kind is what descendants inherit until one
-- of them chooses for itself.
create table issue_components (
  issue_key text primary key references issues(key) on delete cascade,
  mode text not null check (mode in ('explicit', 'none')),
  reason text,
  created_at timestamptz not null default now(),
  updated_at timestamptz not null default now(),
  check (mode <> 'none' or reason is not null)
);

-- A member names a component softly: no foreign key into components, because a
-- re-import may retire the component and the link must survive it — the reader
-- then reports the id as unknown rather than dropping it.
create table issue_component_members (
  issue_key text not null references issue_components(issue_key) on delete cascade,
  project_key text not null,
  component_id text not null,
  primary key (issue_key, component_id)
);

-- Matches the node address the affects arm selects (0032/0037's convention) so
-- a to_id predicate pushed into the arm hits an index.
create index issue_component_members_node on issue_component_members ((project_key || '/' || component_id));

-- graph_edges re-stated: 0037's twelve arms verbatim plus `affects`, the edge
-- from an issue to each component it names explicitly. Inherited attachments
-- are resolved on read and are not edges; created_at is the attachment's
-- last write.
create or replace view graph_edges (from_kind, from_id, kind, to_kind, to_id, created_at, source_seq) as
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
  from ask_followers
  union all
  select 'component', c.project_key || '/' || c.id, 'part_of', 'component', c.project_key || '/' || c.parent, s.imported_at, null
  from components c join architecture_snapshots s on s.id = c.snapshot_id
  where c.parent is not null
  union all
  select 'component', d.project_key || '/' || d.from_id, 'depends_on', 'component', d.project_key || '/' || d.to_id, s.imported_at, null
  from component_depends d
  join components c on c.project_key = d.project_key and c.id = d.from_id
  join architecture_snapshots s on s.id = c.snapshot_id
  union all
  select 'issue', m.issue_key, 'affects', 'component', m.project_key || '/' || m.component_id, c.updated_at, null
  from issue_component_members m join issue_components c using (issue_key);

-- 0037_architecture_projection.up.sql
--
-- The architecture importer's projection: an immutable whole-file snapshot per
-- imported commit, the current component model (delete + reinsert per import),
-- and the component arms of graph_edges. An invalid set never reaches these
-- tables — the importer records the error on architecture_sources.last_error
-- and the previous projection stays up.

-- The architecture directory's own tree object id at the last successful
-- import. A new head commit whose subtree sha is unchanged records the commit
-- without re-fetching or re-projecting the model.
alter table architecture_sources add column last_tree_sha text;

-- One snapshot per (project, commit): the exact files the model was built
-- from, keyed by file name. Re-importing the same commit refreshes
-- architecture_sources.last_sync_at only; the snapshot row never changes.
create table architecture_snapshots (
  id bigserial primary key,
  project_key text not null references projects(key) on delete cascade,
  commit text not null,
  imported_at timestamptz not null default now(),
  files jsonb not null,
  unique (project_key, commit)
);

-- The current model only. parent is a component id in the same project (no FK:
-- the whole set is replaced in one statement order-free, and the parser already
-- rejected unknown parents). paths are repo-relative source paths the
-- component claims.
create table components (
  project_key text not null references projects(key) on delete cascade,
  id text not null,
  title text not null,
  prose text not null,
  parent text,
  external boolean not null default false,
  paths text[] not null default '{}',
  snapshot_id bigint not null references architecture_snapshots(id),
  primary key (project_key, id)
);

create table component_depends (
  project_key text not null,
  from_id text not null,
  to_id text not null,
  primary key (project_key, from_id, to_id),
  foreign key (project_key, from_id) references components (project_key, id) on delete cascade,
  foreign key (project_key, to_id) references components (project_key, id) on delete cascade
);

-- Expression indexes matching the node address each new graph_edges arm
-- selects, so a from_id/to_id predicate pushed into an arm hits an index
-- (0032's convention).
create index components_node on components ((project_key || '/' || id));
create index components_parent_node on components ((project_key || '/' || parent)) where parent is not null;
create index component_depends_from_node on component_depends ((project_key || '/' || from_id));
create index component_depends_to_node on component_depends ((project_key || '/' || to_id));

-- graph_edges re-stated: 0032's ten arms plus the component arms. Component
-- nodes are addressed '<project>/<id>' with kind 'component'; part_of comes
-- from components.parent and depends_on from component_depends. Component
-- edges are projection rows, never rows in refs; created_at is the owning
-- snapshot's import time.
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
  join architecture_snapshots s on s.id = c.snapshot_id;

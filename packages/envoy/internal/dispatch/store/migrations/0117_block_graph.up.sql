-- 0117_block_graph.up.sql
--
-- Every identified block of a document's latest indexed version is a graph node (kind `block`,
-- id `<ref_key>#<block id>`), part_of its document; a mention written inside a block is an edge
-- out of the innermost block holding it. Additive while the previous build serves: new tables, a
-- function no trigger calls yet (0119-0121 attach it), and the view restated with two more arms.
-- Nothing writes the artifacts row: the tables are its children, whose writes take only
-- `for key share` on it.
--
-- A node id holds its document's ref_key, which nothing bounds but artifacts_ref_key_key's own
-- B-tree entry, so with '#' and a block id it can pass the 2704 bytes a B-tree index entry holds.
-- node_id and from_node are hash indexed instead: an entry holds the value's 4-byte hash, and
-- every read of either column is an equality. node_id needs no unique constraint: it is ref_key,
-- '#' and block_id, ref_key is unique and holds no '#', and the primary key makes block_id unique
-- within the document.
create table document_blocks (
  artifact_id uuid not null references artifacts (id) on delete cascade,
  block_id text not null,
  node_id text not null,
  type text not null,
  text_sha256 text not null,
  excerpt text not null,
  changed_version integer not null,
  changed_at timestamptz not null,
  created_at timestamptz not null default now(),
  primary key (artifact_id, block_id)
);
create index document_blocks_node_id on document_blocks using hash (node_id);

-- One row per link a block holds. The link name is part of the key, so two attributes of one
-- block can point at one target under different names.
create table block_refs (
  artifact_id uuid not null references artifacts (id) on delete cascade,
  block_id text not null,
  from_node text not null,
  to_kind text not null,
  to_id text not null,
  kind text not null default 'mentions',
  created_at timestamptz not null default now(),
  created_version integer not null,
  source_seq bigint,
  primary key (artifact_id, block_id, to_kind, to_id, kind)
);
create index block_refs_to on block_refs (to_kind, to_id);
create index block_refs_from_node on block_refs using hash (from_node);

-- The version a document's blocks were indexed at; null is owed: every document at this
-- migration, every version any build writes (0118's trigger), and rebuild-refs. The partial
-- index serves the owed read.
create table document_block_versions (
  artifact_id uuid primary key references artifacts (id) on delete cascade,
  version integer
);
create index document_block_versions_owed on document_block_versions (artifact_id) where version is null;
insert into document_block_versions (artifact_id) select id from artifacts where kind = 'doc';

-- One row each time a block's text changes in an indexed version: new, changed, deleted (text
-- null) or restored. A block's text at version N is the latest row with version <= N, read only
-- when document_indexed_versions holds (artifact_id, N).
create table document_block_texts (
  artifact_id uuid not null references artifacts (id) on delete cascade,
  block_id text not null,
  version integer not null,
  text text,
  primary key (artifact_id, block_id, version)
);

-- Each version a block-aware build indexed. A version an older build wrote and no block-aware
-- build indexed has no row, so no record of its blocks.
create table document_indexed_versions (
  artifact_id uuid not null references artifacts (id) on delete cascade,
  version integer not null,
  primary key (artifact_id, version)
);

-- Comments, asks and issue messages whose text holds `#b-`, owed an index with the block grammar:
-- the triggers of 0119-0121 upsert a row on every write of such a text, 0122 seeds the rows
-- already stored, and the server's block index reconciliation drains them oldest first.
create table block_grammar_owed (
  kind text not null check (kind in ('comment', 'ask', 'message')),
  id uuid not null,
  enqueued_at timestamptz not null default now(),
  primary key (kind, id)
);
create index block_grammar_owed_enqueued on block_grammar_owed (enqueued_at);

-- The one trigger function of 0119-0121: the row's kind is the trigger's argument, and a write
-- that lands on a row already owed moves its enqueued_at, so a drain that read the earlier time
-- leaves the row owed.
create or replace function block_grammar_owe() returns trigger language plpgsql as $$
begin
  insert into block_grammar_owed (kind, id, enqueued_at) values (tg_argv[0], new.id, now())
  on conflict (kind, id) do update set enqueued_at = excluded.enqueued_at;
  return null;
end;
$$;

-- graph_edges re-stated: 0094's arms plus a block's mentions and its part_of edge to its document.
create or replace view graph_edges (from_kind, from_id, kind, to_kind, to_id, created_at, source_seq) as
  select from_kind, from_id, kind, to_kind, to_id, created_at, source_seq from refs
  union all
  select 'issue', key, 'child_of', 'issue', parent_key, created_at, null
  from issues where parent_key is not null
  union all
  select 'issue', issue_key, 'blocked_by', 'issue', target_key, created_at, null
  from issue_links where kind = 'blocked_by'
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
  from issue_component_members m join issue_components c using (issue_key)
  union all
  select 'block', from_node, kind, to_kind, to_id, created_at, source_seq
  from block_refs
  union all
  select 'block', d.node_id, 'part_of', 'artifact', a.ref_key, d.created_at, null
  from document_blocks d join artifacts a on a.id = d.artifact_id;

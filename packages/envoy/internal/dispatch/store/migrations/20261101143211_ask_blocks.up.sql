-- 20261101143211_ask_blocks.up.sql
--
-- An ask is a collection of blocks too, as 0142's comments and messages are, but its blocks are
-- not a body split by the document parser: each part is a text of its own, read from the ask's
-- row by refs.AskParts — its question, then each of its options as "<label> — <description>"
-- (its label alone when it has no description), then the answer it holds now — and its id is
-- kept across writes by the same alignment a comment's blocks take (internal/dispatch/itemblocks).
-- A part is a graph node: its node id is
-- `ask/<ask id>#<part id>` (`ask/<uuid>#b2`), its storage kind in graph_edges and refs.to_kind
-- is `ask_block`, a kind a build without these tables skips as unknown. A part is part_of its
-- ask, a mention written inside it is an edge out of it, and a reply whose request names the
-- ask names the part it answers.
-- Additive while the previous build serves: new tables, no column of an existing table, and the
-- view restated with two more arms and one existing arm split in two (below).
--
-- A node id is `ask/<uuid>#<part id>`, the ask's uuid and the part's own id; the primary key
-- makes it unique within the ask, so it needs no unique constraint. Every read of node_id and
-- from_node is an equality, so both are hash indexed, as 0142's are.
--
-- position is the part's place in refs.AskParts' order - the question, then the options in the
-- ask's row order, then the answer it holds now - null once a later revision removed it,
-- when removed_at holds the time. made_at is the time of the revision that first held the part,
-- null for a part found at the first indexing of an ask edited before this migration, whose
-- making time was never recorded. changed_revision and changed_at are the revision the part's
-- text last changed in and that revision's time.
--
-- made_by is the one column comment_blocks does not have. A comment's blocks are all the
-- comment's author's, but an ask's are not all the asker's: the answer's part was made by whoever
-- answered, who is not the ask's author. Null means the ask's author, which every question and
-- option part is, so only an answer part carries a maker.
create table ask_blocks (
  ask_id uuid not null references asks (id) on delete cascade,
  block_id text not null,
  node_id text not null,
  position integer,
  type text not null,
  text_sha256 text not null,
  excerpt text not null,
  created_at timestamptz not null default now(),
  made_at timestamptz,
  made_by jsonb,
  changed_revision integer not null,
  changed_at timestamptz not null,
  removed_at timestamptz,
  primary key (ask_id, block_id),
  check ((position is null) = (removed_at is not null))
);
create index ask_blocks_node_id on ask_blocks using hash (node_id);
create index ask_blocks_ask_id_text on ask_blocks ((ask_id::text));

-- One row each time a part's text changes in an indexed revision: new, changed, removed (text
-- null) or restored. A part's text at revision N is the latest row with revision <= N, exactly as
-- comment_block_texts reads a comment block's.
create table ask_block_texts (
  ask_id uuid not null references asks (id) on delete cascade,
  block_id text not null,
  revision integer not null,
  text text,
  primary key (ask_id, block_id, revision)
);

-- An ask's indexing state. revision counts its indexed revisions (1 is the first indexing, 0
-- none yet) and next_block is the number of the next block id it mints, so no id is reused.
-- body_sha256 is the question, options and answer last indexed. owed_at is set by
-- 20261101143212's trigger on every write of any build and cleared by the index a block-aware
-- build runs in the same transaction, so a row still owed was written by a build without this
-- index; the partial index serves the reconciliation's read of the oldest owed.
create table ask_block_state (
  ask_id uuid primary key references asks (id) on delete cascade,
  revision integer not null default 0,
  next_block integer not null default 1,
  body_sha256 text,
  owed_at timestamptz
);
create index ask_block_state_owed on ask_block_state (owed_at) where owed_at is not null;

-- One row per link a part of an ask holds, as 0142's comment_block_refs for a comment's block.
-- The _to index ends in the ask's id as text, so the graph's probe of whether a block of the
-- citing item holds a link (refs.heldByABlock, which matches graph_edges.from_id, a text column)
-- is one lookup.
create table ask_block_refs (
  ask_id uuid not null references asks (id) on delete cascade,
  block_id text not null,
  from_node text not null,
  to_kind text not null,
  to_id text not null,
  kind text not null default 'mentions',
  created_at timestamptz not null default now(),
  source_seq bigint,
  primary key (ask_id, block_id, to_kind, to_id, kind)
);
create index ask_block_refs_to on ask_block_refs (to_kind, to_id, (ask_id::text));
create index ask_block_refs_from_node on ask_block_refs using hash (from_node);

-- graph_edges re-stated: 0142's arms plus, for an ask's parts, each current part's part_of edge
-- to its ask and each link a current part holds.
--
-- 0142's reply arm over comments.replies_to_block is split in two. A reply whose request names
-- an ask (ask_id with reply_to_block) names a part of that ask in that same column, whose node
-- id starts `ask/`, where a reply to a comment names a block of that comment, whose node id
-- starts `comment/`, in an ask's thread as anywhere else; the arm as 0142 wrote it published
-- every one of them under comment_block, which would make an ask part's reply an edge to a
-- comment block that does not exist. The node id's prefix tells the two apart, so each reaches
-- its own kind. The messages arm needs no such split: a message never replies to an ask part.
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
  from document_blocks d join artifacts a on a.id = d.artifact_id
  union all
  select 'comment_block', b.node_id, 'part_of', 'comment', b.comment_id::text, b.created_at, null
  from comment_blocks b where b.position is not null
  union all
  select 'message_block', b.node_id, 'part_of', 'message', b.message_id::text, b.created_at, null
  from message_blocks b where b.position is not null
  union all
  select 'ask_block', b.node_id, 'part_of', 'ask', b.ask_id::text, b.created_at, null
  from ask_blocks b where b.position is not null
  union all
  select 'comment_block', r.from_node, r.kind, r.to_kind, r.to_id, r.created_at, r.source_seq
  from comment_block_refs r
  join comment_blocks b on b.comment_id = r.comment_id and b.block_id = r.block_id
  where b.position is not null
  union all
  select 'message_block', r.from_node, r.kind, r.to_kind, r.to_id, r.created_at, r.source_seq
  from message_block_refs r
  join message_blocks b on b.message_id = r.message_id and b.block_id = r.block_id
  where b.position is not null
  union all
  select 'ask_block', r.from_node, r.kind, r.to_kind, r.to_id, r.created_at, r.source_seq
  from ask_block_refs r
  join ask_blocks b on b.ask_id = r.ask_id and b.block_id = r.block_id
  where b.position is not null
  union all
  select 'comment', c.id::text, 'replies_to', 'comment_block', c.replies_to_block, c.created_at, null
  from comments c where c.replies_to_block is not null and c.replies_to_block not like 'ask/%'
  union all
  select 'comment', c.id::text, 'replies_to', 'ask_block', c.replies_to_block, c.created_at, null
  from comments c where c.replies_to_block is not null and c.replies_to_block like 'ask/%'
  union all
  select 'message', m.id::text, 'replies_to', 'message_block', m.replies_to_block, m.created_at, null
  from messages m where m.replies_to_block is not null;

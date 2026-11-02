-- 0142_item_blocks.up.sql
--
-- Every comment and message is a collection of blocks, its body split as the document parser reads
-- it (internal/dispatch/itemblocks). A block is a graph node: its node id is
-- `<item kind>/<item id>#<block id>` (`comment/<uuid>#b2`), its storage kind in graph_edges and
-- refs.to_kind is `comment_block` or `message_block`, a kind a build without these tables skips as
-- unknown. A block is part_of its item, a mention written inside it is an edge out of it, and a
-- reply can name the block it answers. Additive while the previous build serves: new tables, two
-- nullable columns, a function no trigger calls yet (0143 and 0144 attach it, and seed the state
-- rows of the items already stored), and the view restated with six more arms.
--
-- A node id is the item's uuid, '#' and a block id; the primary key makes it unique within the
-- item, so it needs no unique constraint. Every read of node_id and from_node is an equality, so
-- both are hash indexed, as 0121's are.
--
-- position is the block's place in the item's current body, null once a later revision removed
-- it, when removed_at holds the time. made_at is the time of the revision that first held the
-- block, null for a block found at the first indexing of a comment edited before this migration,
-- whose making time was never recorded. changed_revision and changed_at are the revision a
-- comment block's text last changed in and that revision's time.
create table comment_blocks (
  comment_id uuid not null references comments (id) on delete cascade,
  block_id text not null,
  node_id text not null,
  position integer,
  type text not null,
  text_sha256 text not null,
  excerpt text not null,
  created_at timestamptz not null default now(),
  made_at timestamptz,
  changed_revision integer not null,
  changed_at timestamptz not null,
  removed_at timestamptz,
  primary key (comment_id, block_id),
  check ((position is null) = (removed_at is not null))
);
create index comment_blocks_node_id on comment_blocks using hash (node_id);
create index comment_blocks_comment_id_text on comment_blocks ((comment_id::text));

-- A message is never edited, so it has no revisions and no changed_revision: a block's made_at
-- and changed_at are both the message's created_at, and no reader shows a message block's changed
-- facts.
create table message_blocks (
  message_id uuid not null references messages (id) on delete cascade,
  block_id text not null,
  node_id text not null,
  position integer,
  type text not null,
  text_sha256 text not null,
  excerpt text not null,
  created_at timestamptz not null default now(),
  made_at timestamptz not null,
  changed_at timestamptz not null,
  removed_at timestamptz,
  primary key (message_id, block_id),
  check ((position is null) = (removed_at is not null))
);
create index message_blocks_node_id on message_blocks using hash (node_id);
create index message_blocks_message_id_text on message_blocks ((message_id::text));

-- One row each time a comment block's text changes in an indexed revision: new, changed, removed
-- (text null) or restored. A block's text at revision N is the latest row with revision <= N.
create table comment_block_texts (
  comment_id uuid not null references comments (id) on delete cascade,
  block_id text not null,
  revision integer not null,
  text text,
  primary key (comment_id, block_id, revision)
);

-- An item's indexing state. revision counts a comment's indexed revisions (1 is the first
-- indexing, 0 none yet) and next_block is the number of the next block id it mints, so no id is
-- reused. body_sha256 is the body last indexed. owed_at is set by 0143's and 0144's triggers on
-- every write of any build and cleared by the index a block-aware build runs in the same
-- transaction, so a row still owed was written by a build without this index; the partial index
-- serves the reconciliation's read of the oldest owed.
create table comment_block_state (
  comment_id uuid primary key references comments (id) on delete cascade,
  revision integer not null default 0,
  next_block integer not null default 1,
  body_sha256 text,
  owed_at timestamptz
);
create index comment_block_state_owed on comment_block_state (owed_at) where owed_at is not null;

create table message_block_state (
  message_id uuid primary key references messages (id) on delete cascade,
  body_sha256 text,
  owed_at timestamptz
);
create index message_block_state_owed on message_block_state (owed_at) where owed_at is not null;

-- One row per link a block of a comment or message holds, as 0121's block_refs for a document's.
-- The _to index ends in the item's id as text, so the graph's probe of whether a block of the
-- citing item holds a link (refs.heldByABlock, which matches graph_edges.from_id) is one lookup.
create table comment_block_refs (
  comment_id uuid not null references comments (id) on delete cascade,
  block_id text not null,
  from_node text not null,
  to_kind text not null,
  to_id text not null,
  kind text not null default 'mentions',
  created_at timestamptz not null default now(),
  source_seq bigint,
  primary key (comment_id, block_id, to_kind, to_id, kind)
);
create index comment_block_refs_to on comment_block_refs (to_kind, to_id, (comment_id::text));
create index comment_block_refs_from_node on comment_block_refs using hash (from_node);

create table message_block_refs (
  message_id uuid not null references messages (id) on delete cascade,
  block_id text not null,
  from_node text not null,
  to_kind text not null,
  to_id text not null,
  kind text not null default 'mentions',
  created_at timestamptz not null default now(),
  source_seq bigint,
  primary key (message_id, block_id, to_kind, to_id, kind)
);
create index message_block_refs_to on message_block_refs (to_kind, to_id, (message_id::text));
create index message_block_refs_from_node on message_block_refs using hash (from_node);

-- The block node id a reply answers, null for a reply to the whole item. A catalog change that
-- rewrites and scans no row; the partial indexes serve the reads of a block's replies.
alter table comments add column replies_to_block text;
alter table messages add column replies_to_block text;
create index comments_replies_to_block on comments (replies_to_block) where replies_to_block is not null;
create index messages_replies_to_block on messages (replies_to_block) where replies_to_block is not null;

-- The one trigger function of 0143 and 0144: the item's kind is the trigger's argument. A write
-- that lands on a state row moves only its owed_at, never its revision or next_block, so a drain
-- that read the earlier time leaves the row owed.
create or replace function item_blocks_owe() returns trigger language plpgsql as $$
begin
  if tg_argv[0] = 'comment' then
    insert into comment_block_state (comment_id, owed_at) values (new.id, now())
    on conflict (comment_id) do update set owed_at = excluded.owed_at;
  elsif tg_argv[0] = 'message' then
    insert into message_block_state (message_id, owed_at) values (new.id, now())
    on conflict (message_id) do update set owed_at = excluded.owed_at;
  else
    raise exception 'item_blocks_owe: unknown item kind %', tg_argv[0];
  end if;
  return null;
end;
$$;

-- graph_edges re-stated: 0121's arms plus, for comment and message blocks, each current block's
-- part_of edge to its item, each link a current block holds, and a reply's replies_to edge to the
-- block it names.
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
  select 'comment', c.id::text, 'replies_to', 'comment_block', c.replies_to_block, c.created_at, null
  from comments c where c.replies_to_block is not null
  union all
  select 'message', m.id::text, 'replies_to', 'message_block', m.replies_to_block, m.created_at, null
  from messages m where m.replies_to_block is not null;

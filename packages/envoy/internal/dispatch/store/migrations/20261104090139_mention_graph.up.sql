-- Identity mentions - a comment, a message or a document block naming a person, a session or a
-- role - are stored in a table of their own, and in no existing relation.
--
-- They have to be. Every build reconciles the references of a source it writes by deleting the
-- rows its own parser did not find, and two of the three reconciliations are scoped by the owner
-- alone, not by the kind of the row: a document version deletes every block_refs row of its
-- artifact (refs/blocks.go), and an item's indexing every <item>_block_refs row of its item
-- (refs/item_blocks.go). So no storage kind can hide an identity mention from a build that does
-- not know identity mentions, and during an ordinary rolling deploy an old writer would delete
-- the rows a new writer had just made and then mark the source indexed, with nothing to revisit
-- it. A table an old build never names cannot be reconciled by it, so every row survives the
-- overlap, and a rollback leaves them in place for the next deploy.
--
-- Old readers see nothing new either: graph_edges is unchanged and holds none of this, so no
-- existing read gains a row or changes a plan.
--
-- owner_kind/owner_id is the reconciliation that owns the row - the source whose next write
-- replaces the rows it no longer finds. from_kind/from_id is the graph node the mention is from,
-- which for a block is its node id. The two differ only for blocks: a document version owns its
-- blocks' rows, an item its parts'.
create table identity_mentions (
  owner_kind text not null,
  owner_id   text not null,
  from_kind  text not null,
  from_id    text not null,
  to_kind    text not null check (to_kind in ('person', 'session', 'role')),
  to_id      text not null,
  created_at timestamptz not null default now(),
  source_seq bigint,
  primary key (owner_kind, owner_id, from_kind, from_id, to_kind, to_id)
);

-- One identity's mentions, newest first: the read behind a person's page and every ?to= query.
create index identity_mentions_target on identity_mentions (to_kind, to_id, created_at desc);
create index identity_mentions_source on identity_mentions (from_kind, from_id);

-- graph_mention_resolutions is every record of an identity mention, before the person expansion:
-- the indexed body mentions above, and the explicit comment mention targets and resolutions that
-- the old mention path already records. A plain direct message or broadcast copy is not here: it
-- is a delivery, which graph_person_edges and graph_inbox_edges already carry as delivered_to, and
-- counting it as a mention showed every direct message on a person's page twice, once as Received
-- and once as Mentioned in. A message is a mention source only when its own body names an
-- identity.
create view graph_mention_resolutions (from_kind, from_id, to_kind, to_id, created_at, source_seq) as
    select 'comment'::text as from_kind, m.from_id, m.to_kind, m.to_id, c.created_at, m.source_seq
    from identity_mentions m join comments c on c.id::text = m.from_id
    where m.from_kind = 'comment'
    union all
    select 'message', m.from_id, m.to_kind, m.to_id, s.created_at, m.source_seq
    from identity_mentions m join messages s on s.id::text = m.from_id
    where m.from_kind = 'message'
    union all
    -- A document block's mention is dated by the mention itself, never by document_blocks.changed_at:
    -- a row that survives a version keeps its created_at, so an old mention stays where it is when
    -- its block is edited around it.
    select 'block', m.from_id, m.to_kind, m.to_id, m.created_at, m.source_seq
    from identity_mentions m join document_blocks d on d.node_id = m.from_id
    where m.from_kind = 'block'
    union all
    select 'comment', m.comment_id::text, target.kind, substring(m.target from length(target.prefix) + 1), c.created_at, null
    from comment_mentions m join comments c on c.id = m.comment_id
    cross join (values ('session', 'session:'), ('role', 'role:')) target(kind, prefix)
    where starts_with(m.target, target.prefix)
    union all
    select 'comment', m.comment_id::text, 'session', m.resolved_session_id, c.created_at, null
    from comment_mentions m join comments c on c.id = m.comment_id
    where m.resolved_session_id <> '' and m.target <> 'session:' || m.resolved_session_id
    union all
    select 'comment', d.comment_id::text, 'session', d.session_id, c.created_at, null
    from comment_deliveries d join comments c on c.id = d.comment_id
    where d.session_id <> '' and (d.resolve_error is null or d.resolve_error not like 'no live session %')
;

create view graph_mention_edges (from_kind, from_id, kind, to_kind, to_id, created_at, source_seq) as
  select from_kind, from_id, 'mentions', to_kind, to_id, min(created_at), min(source_seq)
  from (
    select * from graph_mention_resolutions
    union all
    select r.from_kind, r.from_id, 'person', a.person, r.created_at, r.source_seq
    from graph_mention_resolutions r join person_aliases a on a.alias_kind = 'session' and a.alias = r.to_id
    where r.to_kind = 'session'
  ) expanded
  where to_id <> '' and to_kind in ('person', 'session', 'role')
  group by from_kind, from_id, to_kind, to_id;

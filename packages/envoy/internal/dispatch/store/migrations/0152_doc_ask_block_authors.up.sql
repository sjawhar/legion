-- 0152: the author of an ask block no settlement has indexed yet outlives a room and a process.
-- A settlement indexes a new ask block with the author of the write that introduced
-- it, and until this table only the room that saw that write knew who it was, so a settlement on
-- another envoy-dispatch process, or on the process that resumed the document after the writer's
-- crashed, named its own actor instead.
--
-- A row is written where the author is known, in the transaction that makes the block durable: a
-- browser's block in the append of the update whose render first showed it, a joined write's or a
-- seed's in the transaction that commits it, a block a stamp renamed in the transaction that
-- stores the stamp. The first registration wins (on conflict do nothing). A settlement on any
-- process reads the author from this table, and deletes the rows of the blocks it indexes or
-- finds indexed, and of the blocks no longer in the document, in its own transaction. Every one
-- of those writes holds the document's advisory lock (lockDocumentRoom). The release before this
-- one neither writes nor deletes a row; its asks name the settlement's actor as they always
-- have, and this release's next settlement deletes the rows of blocks that release indexed
-- meanwhile.
--
-- A block id is any length a browser's edit makes it, and a B-tree entry holds at most 2704 bytes,
-- so a block is keyed by block_id_key(block_id), the id's SHA-256 (0127), not by the id itself: an
-- incompressible id past that would fail the append that records it, and the browser's edit with
-- it. Two ids under one key would be a SHA-256 collision.
--
-- The migration adds this table and its index and nothing else, so the release before this one
-- runs over the new schema unchanged and the census answers 0. It takes ACCESS EXCLUSIVE on the
-- new table, which nothing else reads yet, and SHARE ROW EXCLUSIVE on artifacts for the foreign
-- key to it (on delete cascade): a write to artifacts waits behind the migration's short
-- transaction, and a read does not.
create table doc_ask_block_authors (
  artifact_id uuid not null references artifacts(id) on delete cascade,
  block_id text not null,
  actor jsonb not null,
  recorded_at timestamptz not null default now()
);
create unique index doc_ask_block_authors_block
  on doc_ask_block_authors (artifact_id, block_id_key(block_id));

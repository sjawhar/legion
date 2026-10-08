-- 0083_doc_pending_authors.up.sql
-- LEGION-513: a version's pending authors outlive a room and a process.
--
-- A version lists the authors whose changes it holds (LEGION-503). An author whose durable change
-- no committed version lists yet has one row here: a browser update's append writes it in the
-- update's own transaction, a joined write in the transaction that commits its content, and a
-- committed version deletes the rows it lists. Every one of those writes holds the document's
-- advisory lock (lockDocumentRoom), so a version that reads the rows under that lock lists each
-- author once. An actor key joins its kind and id with a NUL byte, which text refuses, so the key
-- is stored as the two columns.
create table doc_pending_authors (
  artifact_id uuid not null references artifacts(id) on delete cascade,
  actor_kind text not null,
  actor_id text not null,
  actor jsonb not null,
  primary key (artifact_id, actor_kind, actor_id)
);

-- The latest edit source of a document whose settlement is owed: the settlement names it on its
-- events, and a room loaded after a restart or an issue's reopening restores it. Existing rows
-- have none: before this migration a restart already lost it.
alter table doc_settlements_pending
  add column last_actor jsonb;

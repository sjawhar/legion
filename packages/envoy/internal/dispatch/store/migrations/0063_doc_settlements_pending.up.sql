-- 0063_doc_settlements_pending.up.sql
-- LEGION-465: a document whose settlement is still owed says so in the database.
--
-- Settlement (docs.Service.settleRoom) versions a document and indexes its ask blocks after the
-- updates a write appends. It was armed only in memory, so a shutdown whose budget ended before a
-- large document's settlement finished dropped it, and the restarted server armed none until the
-- next edit. Every durable document update now records here, in its own transaction, the update
-- version a settlement still owes; the settlement that covers it deletes the row in the
-- transaction that commits its writes, and a room's load settles once when its row is here.
-- A new table: no existing row is read, refused or rewritten.
create table doc_settlements_pending (
  artifact_id uuid primary key references artifacts(id),
  through_version bigint not null check (through_version > 0),
  marked_at timestamptz not null default now()
);

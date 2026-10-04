-- 0063_doc_settlements_pending.up.sql
-- LEGION-465: a document whose settlement is still owed says so in the database.
--
-- Settlement (docs.Service.settleRoom) versions a document and indexes its ask blocks after the
-- updates a write appends. Its timer lives in memory, so a settlement a shutdown cuts short would
-- otherwise be lost. Every durable document update records here, in its own transaction, that a
-- settlement is still owed; the settlement that covers it deletes the row in the transaction that
-- commits its writes, and a room's load and the server's resumption settle a document whose row is
-- here. The row is derived from the document's updates, so it goes with the document. A new table:
-- no existing row is read, refused or rewritten.
create table doc_settlements_pending (
  artifact_id uuid primary key references artifacts(id) on delete cascade,
  marked_at timestamptz not null default now()
);

create index doc_settlements_pending_marked_at on doc_settlements_pending (marked_at);

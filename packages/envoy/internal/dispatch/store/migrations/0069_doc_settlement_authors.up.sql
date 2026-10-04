-- 0069_doc_settlement_authors.up.sql
-- LEGION-513: a settlement's authors must outlive a room and a process.
--
-- A document update already leaves doc_settlements_pending as the durable record that its
-- settlement is owed. The settlement's pending version authors and its latest actor used to live
-- only in docs.roomState, so closing an issue before the settle delay released them and a restart
-- did too. The row now carries the same credit until the settlement that consumes it deletes the
-- row. Existing rows have no authors to preserve: before this migration a process restart already
-- made their attribution unavailable.
alter table doc_settlements_pending
  add column settlement_authors jsonb not null default '{"pending": {}}';

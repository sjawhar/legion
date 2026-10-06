-- 0079_doc_settlement_authors.up.sql
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

-- A room's own generation (roomState.creditGeneration) is a per-room-load instance id, not a
-- fact two processes can compare without a durable, time-bounded record of which generation is
-- still live: this table is that record. A live room holds a lease on its own generation -
-- refreshed on a ticker while it stays live, deleted when it unloads cleanly - so a different
-- room loading or sweeping the same document can tell a still-live generation's entries (whose
-- lease has not expired: leave them, the live room's own release will reach them) from an
-- abandoned one's (expired or never leased: adopt them into this room's own generation, or they
-- would stay stuck under a generation nothing will ever release again).
create table doc_settlement_generation_leases (
  artifact_id uuid not null references artifacts(id) on delete cascade,
  generation bigint not null,
  expires_at timestamptz not null,
  primary key (artifact_id, generation)
);

create index doc_settlement_generation_leases_expires_at on doc_settlement_generation_leases (expires_at);

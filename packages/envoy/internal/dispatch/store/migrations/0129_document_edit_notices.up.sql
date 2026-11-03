-- A person's pending edit notice, one row per document. When a person's browser edit
-- settles into a version, the settlement opens this row, or moves it on, in its own transaction
-- (docs.recordEditNotice): from_version is the version before the first person's version of the
-- quiet window, to_version the last one, editors every person who wrote one, last_editor the
-- actor the notice's event carries, and due_at the end of the quiet window, which each later
-- person's version moves. A leased sweep (docs.RunEditNotices) deletes each due row and appends
-- one artifact.human_edited event for it in the same transaction. The window is a row, not a
-- timer in memory, so a notice still pending when a process stops is sent by the next sweep of
-- any process.
--
-- A row with no last_editor and no due_at is a window no person can be named for yet: its
-- versions list people, but the sender of each one's latest edit is unknown (two peers were
-- connected). The sweep never sends it. The next version whose latest edit has a known person
-- sender names that person and sets due_at, and its notice then covers every version of the row.
-- The check holds the two columns together.
--
-- A new table: no existing row is read or rewritten, and the release before this one neither
-- reads nor writes it. It is not lock-free, though: the foreign key takes ShareRowExclusiveLock
-- on artifacts for the length of the migration, which holds off writes to artifacts (an
-- outside reviewer of #2137 measured about 11 ms for the table alone). The migration is one
-- transaction, so that lock is still held while the alter below waits for ACCESS EXCLUSIVE on
-- doc_settlements_pending, which every document append writes: a long transaction holding that
-- table can hold artifacts writes and every append behind it for up to pgmigrate.LockTimeout's
-- five seconds, after which the migration fails the boot. The foreign key's on
-- delete cascade keeps that release's artifact deletes working: a deleted document takes its
-- pending notice with it.
create table document_edit_notices (
  artifact_id uuid primary key references artifacts(id) on delete cascade,
  from_version integer not null,
  to_version integer not null,
  editors jsonb not null default '[]'::jsonb,
  last_editor jsonb,
  due_at timestamptz,
  constraint document_edit_notices_named_when_due check ((last_editor is null) = (due_at is null))
);
create index document_edit_notices_due on document_edit_notices (due_at);

-- doc_settlements_pending.last_person: the latest person sender of the document's pending edits,
-- which a version's edit notice names (docs.latestEditSource). last_actor is the sender of the
-- latest edit of any kind, so an agent's edit after a person's replaces it; last_person is
-- written by the same writers under the same advisory lock, by one rule: a person sender sets it,
-- an edit whose sender is unknown and which credits a person clears it, and a session sender, or
-- an unknown sender crediting only sessions, leaves it as it is. Closing an issue does not touch
-- it. A plain nullable add: it rewrites no row, and the ALTER takes ACCESS EXCLUSIVE on
-- doc_settlements_pending only for the catalog write. The release before this one never reads or
-- writes the column, so an update one of its pods appends leaves it as it was.
alter table doc_settlements_pending add column last_person jsonb;

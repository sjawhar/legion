-- INFRA-140: a person's pending edit notice, one row per document. When a person's browser edit
-- settles into a version, the settlement opens this row, or moves it on, in its own transaction
-- (docs.recordEditNotice): from_version is the version before the first person's version of the
-- quiet window, to_version the last one, editors every person who wrote one, last_editor the
-- actor the notice's event carries, and due_at the end of the quiet window, which each later
-- person's version moves. A leased sweep (docs.RunEditNotices) deletes each due row and appends
-- one artifact.human_edited event for it in the same transaction. The window is a row, not a
-- timer in memory, so a notice still pending when a process stops is sent by the next sweep of
-- any process.
--
-- A new table: nothing existing is read, locked or rewritten, and the release before this one
-- neither reads nor writes it. The foreign key's on delete cascade keeps that release's artifact
-- deletes working: a deleted document takes its pending notice with it.
create table document_edit_notices (
  artifact_id uuid primary key references artifacts(id) on delete cascade,
  from_version integer not null,
  to_version integer not null,
  editors jsonb not null default '[]'::jsonb,
  last_editor jsonb not null,
  due_at timestamptz not null
);
create index document_edit_notices_due on document_edit_notices (due_at);

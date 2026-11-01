-- 0164_document_search_backfill.up.sql
-- How far the fill of 0162's document_search has got, and nothing else: one row holding the
-- keyset cursor and whether the pass is finished. This migration writes no document_search row
-- and reads no large table, so it takes no lock that grows with any table and commits in
-- milliseconds.
--
-- The fill itself is store.FillDocumentSearch, which the boot runs in committed batches after
-- the migrations and before the server serves (cmd/dispatch/main.go). It is not a migration
-- because a migration is one transaction (store/store.go applyMigrationOnce), and the one
-- statement this file used to carry - `insert … select distinct on (artifact_id) … from
-- artifact_versions` - measured 35.5 s on a production-shaped database (artifact_versions
-- 3337 MB, 200,000 versions, 40,000 documents), with every row it wrote uncommitted for those
-- 35.5 s. 0163's trigger is live by the time it runs, so a version write to a document the
-- statement had already written ran `insert … on conflict (artifact_id) do update` on that key
-- and waited for the whole statement. That writer holds its issue row (`for no key update`) and
-- its document's room for the whole wait, every other write on that issue queues behind it, and
-- an API writer sets no lock_timeout. A committed batch holds such a key for one batch instead.
--
-- One row, as 0154's cursor is one row, and the same singleton primary key keeps it one.
-- artifact_id is the keyset cursor: the highest artifact_id the pass has covered, and the
-- all-zero uuid before its first batch, which sorts below every id the database generates
-- because Postgres orders a uuid by its sixteen bytes. done is what an ordinary boot reads and
-- stops on, so a boot after the pass has finished costs this one row and no read of
-- artifact_versions at all.
create table document_search_backfill (
  singleton boolean primary key default true check (singleton),
  artifact_id uuid not null default '00000000-0000-0000-0000-000000000000',
  done boolean not null default false
);
insert into document_search_backfill default values;

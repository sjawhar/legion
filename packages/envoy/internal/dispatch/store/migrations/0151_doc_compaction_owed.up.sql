-- 0151_doc_compaction_owed.up.sql
-- INFRA-593: the documents with rows a compaction could fold, one row each, so the hourly
-- compaction sweep (docs.Service.compactionSweepPass) reads only them. Listing them from
-- doc_updates itself reads at least one entry per document every hour, by its heap or its primary
-- key, and doc_updates cannot take a migration that would add an index or a column for it: the
-- pre-deploy census refuses a migration that locks a table of 1 GiB or more, and doc_updates may be
-- that large.
--
-- Every durable append (docs.PgVersioned.appendUpdateTxClass) records its document here, in the
-- statement that inserts its row, when that row is not the document's first; a document already
-- here is left as it is. A compaction that leaves no row above its fold point deletes the
-- document's row in its write's transaction, and one that leaves younger rows keeps it. A rebuild,
-- which leaves one row, and a document's deletion delete it too. Every one of those writes holds the
-- document's advisory lock, so none races another. The sweep deletes a row whose document holds no
-- row after its first, as a prune or an artifact's deletion (which cascades to doc_updates, not
-- here) leaves it. No foreign key reaches artifacts: it would lock artifacts on every append that
-- records a document.
--
-- The backfill names every document holding more than one row now. It reads doc_updates (ACCESS
-- SHARE, which the census does not count as touching it) and writes only the table it creates.
--
-- The release before this one neither reads nor writes the table. A document that only that release
-- appends to during the roll, after the backfill, is not named here; the new release's compaction
-- queue compacts it on its next append or load, and that append names it.
create table doc_compaction_owed (
  artifact_id uuid primary key
);

insert into doc_compaction_owed (artifact_id)
select artifact_id from doc_updates group by artifact_id having count(*) > 1;

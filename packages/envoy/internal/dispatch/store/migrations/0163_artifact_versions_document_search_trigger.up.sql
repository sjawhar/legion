-- 0163_artifact_versions_document_search_trigger.up.sql
-- Keeps 0162's document_search in step with artifact_versions: every version any build writes
-- replaces its document's row, unless the row already names a higher version.
--
-- A function and a trigger alone, and nothing else, so the pre-deploy census exempts the
-- migration from its table-size limit (internal/pgmigrate/exempt.go tableSizeExemption): neither
-- statement reads a row, and artifact_versions, at 5.2 GiB, is far above that limit. Its own
-- migration for the same reason (0122's precedent), so the transaction holds this table's lock
-- alone.
--
-- It fires AFTER, so 0058's BEFORE trigger has filled new.search by the time it reads it.
-- `update of search` is in the column list deliberately: a writer that sets
-- artifact_versions.search directly (api/related_recall_test.go empties and refills it) would
-- otherwise leave this table holding what the markdown says, behind what the version holds.
--
-- `when (new.markdown is not null)` for the reason 0122's trigger carries the same guard: a
-- column list restricts an update but never an insert, so a version with no markdown - an image
-- or a file upload - fires this trigger too, and its search is null, every function of 0058's
-- expression being strict. Its number being the highest, it would blank the document's row. The
-- guard leaves the document's last version with text standing instead, which is the row search
-- reads.
--
-- `where excluded.number >= document_search.number` is what keeps the latest version's vector:
-- a version whose transaction opened before a higher-numbered one and committed after it, and an
-- edit of an old version's markdown, both leave the higher number in place. `>=` rather than `>`
-- so that a rewrite of the version the row already names still updates it.
--
-- No filter on the artifact's kind: that would read artifacts inside the trigger on every
-- version write, and a row for an artifact that is not a doc is harmless, since every reader of
-- document_search joins artifacts on a.kind = 'doc'.
create or replace function artifact_versions_document_search() returns trigger language plpgsql as $$
begin
  insert into document_search (artifact_id, number, created_at, search)
  values (new.artifact_id, new.number, new.created_at, new.search)
  on conflict (artifact_id) do update
     set number = excluded.number, created_at = excluded.created_at, search = excluded.search
   where excluded.number >= document_search.number;
  return null;
end;
$$;
create trigger artifact_versions_document_search after insert or update of markdown, search on artifact_versions
  for each row when (new.markdown is not null) execute function artifact_versions_document_search();

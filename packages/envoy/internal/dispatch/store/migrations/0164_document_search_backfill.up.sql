-- 0164_document_search_backfill.up.sql
-- Fills 0162's document_search for every document already stored: the latest version of each
-- artifact that carries markdown, which is the version search reads.
--
-- It reads artifact_versions and writes no row of it, so the pre-deploy census does not size that
-- table (internal/pgmigrate/touched.go leaves out a table a statement only reads) and the
-- migration is not refused over its 5.2 GiB. The read takes ACCESS SHARE, which waits only behind
-- an ACCESS EXCLUSIVE holder, and `distinct on` walks
-- artifact_versions_artifact_id_number_key in order rather than sorting the table.
--
-- `markdown is not null` is 0163's trigger's own guard: a version with no markdown has a null
-- search vector and is no document to search.
--
-- The same write guard as that trigger, because 0163 applies before this migration: a version
-- written between the two is already here, with a number at least as high as the one this read
-- finds, and must not be replaced by an older one.
insert into document_search (artifact_id, number, created_at, search)
select distinct on (v.artifact_id) v.artifact_id, v.number, v.created_at, v.search
  from artifact_versions v
 where v.markdown is not null
 order by v.artifact_id, v.number desc
on conflict (artifact_id) do update
   set number = excluded.number, created_at = excluded.created_at, search = excluded.search
 where excluded.number >= document_search.number;

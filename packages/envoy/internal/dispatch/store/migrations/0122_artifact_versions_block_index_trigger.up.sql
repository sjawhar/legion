-- 0122_artifact_versions_block_index_trigger.up.sql
-- Every document version any build writes marks its document's blocks owed (0121's
-- document_block_versions), so a version a build that predates the block index writes during the
-- roll is indexed by the reconciliation, and a block-aware build's own index, in the same
-- transaction after the insert, leaves the row indexed. An image or file version carries no
-- markdown and owes nothing. Its own migration, so the transaction holds artifact_versions' lock
-- alone (0074's precedent).
create or replace function document_block_versions_owe() returns trigger language plpgsql as $$
begin
  insert into document_block_versions (artifact_id, version) values (new.artifact_id, null)
  on conflict (artifact_id) do update set version = null;
  return null;
end;
$$;
create trigger artifact_versions_block_index_owed after insert on artifact_versions
  for each row when (new.markdown is not null) execute function document_block_versions_owe();

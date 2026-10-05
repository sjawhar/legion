-- 0073_artifact_versions_embeddings_trigger.up.sql
-- Its own migration, so the transaction holds artifact_versions' ACCESS EXCLUSIVE lock alone, for
-- milliseconds, and never while waiting for another table's.
--
-- artifact_versions is insert-only (immutable versions - model.go "Version"); each new version
-- re-embeds the artifact it belongs to, at its own id, matching the keyword leg's "only the
-- latest version is searched" (search.go's document leg). An image or file version carries no
-- markdown and enqueues nothing.
create or replace function embeddings_enqueue_document() returns trigger language plpgsql as $$
begin
  if new.markdown is not null then
    perform embeddings_enqueue('document', new.artifact_id::text, new.markdown);
  end if;
  return new;
end;
$$;
create trigger artifact_versions_embeddings_enqueue after insert on artifact_versions
  for each row execute function embeddings_enqueue_document();

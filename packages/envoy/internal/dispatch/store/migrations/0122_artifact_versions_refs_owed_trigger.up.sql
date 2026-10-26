-- 0122_artifact_versions_refs_owed_trigger.up.sql
-- INFRA-189: a document version owes its document a re-index (0118), and owes the document as a
-- scope: a version changes the document's question map (refs.QuestionMap) without any ask row
-- moving when it drops or renames a block, or when settlement writes a copied block's copied_from
-- into it, so every `#id` in the document's scope is read again. artifact_versions is insert-only;
-- an image or file version carries no markdown, is never indexed, and owes nothing. Its own
-- migration, so the transaction holds artifact_versions' lock alone (0074's precedent).
create function refs_owed_version() returns trigger language plpgsql as $$
begin
  perform refs_owed_enqueue('artifact', new.artifact_id::text);
  perform refs_owed_enqueue('scope', new.artifact_id::text);
  return null;
end;
$$;

create trigger artifact_versions_refs_owed after insert on artifact_versions
  for each row when (new.markdown is not null) execute function refs_owed_version();

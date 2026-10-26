-- 0122_artifact_versions_refs_owed_trigger.up.sql
-- INFRA-189: a document version owes its document a re-index (0118). It owes the document as a
-- scope too, so every `#id` in the document's scope is read again, only when it changes the
-- directive lines the document's question map is read from (refs.QuestionMap): a version that adds,
-- drops or renames a block, or into which settlement writes a copied block's copied_from, changes
-- the map without any ask row moving. A scope re-reads every source of the document that may cite a
-- question, so a version whose question blocks stand as they were - nearly every edit - owes none.
-- The lines compared are every line holding a `:::ask{` opener (three or more colons), sorted, of
-- this version and of the document's previous version holding markdown (none before the first):
-- more than refs.questionBlocks reads, which also wants the line to be the directive and to name
-- an id, so any change it can see changes these lines. Each line matching `:{3,}ask\{` holds
-- `:::ask{`, so a version without that text - most documents, which declare no question block - has
-- none and is never split into lines. artifact_versions is insert-only; an image or file version
-- carries no markdown, is never indexed, and owes nothing. Its own migration, so the transaction
-- holds artifact_versions' lock alone (0074's precedent).
create function refs_owed_version() returns trigger language plpgsql as $$
declare
  directives text[] := '{}';
  previous text[] := '{}';
  earlier text;
begin
  perform refs_owed_enqueue('artifact', new.artifact_id::text);
  if strpos(new.markdown, ':::ask{') > 0 then
    directives := array(
      select line from regexp_split_to_table(new.markdown, E'\n') as line
      where line ~ ':{3,}ask\{'
      order by line
    );
  end if;
  select markdown into earlier from artifact_versions
  where artifact_id = new.artifact_id and number < new.number and markdown is not null
  order by number desc limit 1;
  if strpos(earlier, ':::ask{') > 0 then
    previous := array(
      select line from regexp_split_to_table(earlier, E'\n') as line
      where line ~ ':{3,}ask\{'
      order by line
    );
  end if;
  if directives is distinct from previous then
    perform refs_owed_enqueue('scope', new.artifact_id::text);
  end if;
  return null;
end;
$$;

create trigger artifact_versions_refs_owed after insert on artifact_versions
  for each row when (new.markdown is not null) execute function refs_owed_version();

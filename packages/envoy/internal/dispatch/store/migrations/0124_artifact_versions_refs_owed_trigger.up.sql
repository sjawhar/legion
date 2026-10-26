-- 0124_artifact_versions_refs_owed_trigger.up.sql
-- INFRA-189: a document version owes its document a re-index (0120). It owes the document as a
-- scope too, so every `#id` in the document's scope is read again, only when it changes the
-- directive lines the document's question map is read from (refs.QuestionMap): a version that adds,
-- drops or renames a block, or into which settlement writes a copied block's copied_from, changes
-- the map without any ask row moving. A scope re-reads every source of the document that may cite a
-- question, so a version whose question blocks stand as they were - nearly every edit - owes none.
-- The lines compared, in document order, are every line holding a `:::ask{` opener (three or more
-- colons) or a run of three or more backticks or tildes, of this version and of the document's
-- previous version holding markdown (none before the first). refs.questionBlocks reads a question
-- from the directive lines it finds outside fenced code, and which lines are fenced depends only on
-- the fence lines and their order, so any change it can see - a block added, dropped, renamed or
-- given copied_from, or a fence opened or closed around a block - changes these lines. The set is
-- larger than what it reads: a fenced example, a reordered block or an edited code block in a
-- document that declares a question owes at most a scope re-read that finds nothing new. A version
-- without `:::ask{` - most documents, which declare no question block - compares as no lines and is
-- never split. artifact_versions is insert-only; an image or file version carries no markdown, is
-- never indexed, and owes nothing. Its own migration, so the transaction holds artifact_versions'
-- lock alone (0074's precedent).
create function refs_owed_version() returns trigger language plpgsql as $$
declare
  directives text[] := '{}';
  previous text[] := '{}';
  earlier text;
begin
  perform refs_owed_enqueue('artifact', new.artifact_id::text);
  if strpos(new.markdown, ':::ask{') > 0 then
    directives := array(
      select line from regexp_split_to_table(new.markdown, E'\n') with ordinality as t(line, n)
      where line ~ ':{3,}ask\{' or line ~ '(`{3}|~{3})'
      order by n
    );
  end if;
  select markdown into earlier from artifact_versions
  where artifact_id = new.artifact_id and number < new.number and markdown is not null
  order by number desc limit 1;
  if strpos(earlier, ':::ask{') > 0 then
    previous := array(
      select line from regexp_split_to_table(earlier, E'\n') with ordinality as t(line, n)
      where line ~ ':{3,}ask\{' or line ~ '(`{3}|~{3})'
      order by n
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

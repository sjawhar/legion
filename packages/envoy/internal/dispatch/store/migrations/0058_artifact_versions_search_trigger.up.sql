-- 0058_artifact_versions_search_trigger.up.sql
-- LEGION-465 part 3 of 7: artifact_versions.search is filled by a trigger, not generated.
--
-- The generated column cannot take the new expression in place on Postgres 16 (SET EXPRESSION is
-- Postgres 17), and dropping and re-adding one rewrites the table under ACCESS EXCLUSIVE for as
-- long as every row's vector takes (29 s per 100 MB of text). DROP EXPRESSION is a catalog change
-- that keeps every value, so the column becomes a plain one that this BEFORE trigger fills on every
-- insert and on every update of the markdown, exactly as 0019's generated expression did
-- (to_tsvector('english', regexp_replace(markdown, '(?s):::ask\{.*?:::', '', 'g'))) with
-- search_text applied. No application code writes search. Its own migration, so the transaction
-- holds this table's ACCESS EXCLUSIVE lock alone, for milliseconds, and never while waiting for
-- another table's; 0062 re-indexes the rows the new expression changes.
--
-- 0010 wrote that expression with coalesce(markdown, '') and 0019 rewrote it without, so today a
-- binary version - whose markdown is NULL - has a NULL vector, every function in the expression
-- being STRICT, and an empty markdown has the empty vector. The search reads documents alone
-- (a.kind = 'doc'), so neither value is ever matched; the trigger keeps 0019's, and
-- TestSearchTextMigrationKeepsEveryRowShapesVector holds every shape to the generated column's output.
alter table artifact_versions alter column search drop expression;
create function artifact_versions_search() returns trigger language plpgsql as $$
begin
  new.search := to_tsvector('english', regexp_replace(search_text(new.markdown), '(?s):::ask\{.*?:::', '', 'g'));
  return new;
end $$;
create trigger artifact_versions_search before insert or update of markdown on artifact_versions
  for each row execute function artifact_versions_search();

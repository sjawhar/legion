-- 0060_asks_search_trigger.up.sql
-- LEGION-465 part 5 of 7: asks.search is filled by a trigger, not generated.
--
-- The generated column cannot take the new expression in place on Postgres 16 (SET EXPRESSION is
-- Postgres 17), and dropping and re-adding one rewrites the table under ACCESS EXCLUSIVE for as
-- long as every row's vector takes (29 s per 100 MB of text). DROP EXPRESSION is a catalog change
-- that keeps every value, so the column becomes a plain one that this BEFORE trigger fills on every
-- insert and on every update of the question, options or answer, exactly as 0019's generated
-- expression did (to_tsvector('english', question || ' ' || ask_search_suffix(options, answer)))
-- with search_text applied. No application code writes search. Its own migration, so the
-- transaction holds this table's ACCESS EXCLUSIVE lock alone, for milliseconds, and never while
-- waiting for another table's; 0062 re-indexes the rows the new expression changes.
alter table asks alter column search drop expression;
create function asks_search() returns trigger language plpgsql as $$
begin
  new.search := to_tsvector('english', search_text(new.question || ' ' || ask_search_suffix(new.options, new.answer)));
  return new;
end $$;
create trigger asks_search before insert or update of question, options, answer on asks
  for each row execute function asks_search();

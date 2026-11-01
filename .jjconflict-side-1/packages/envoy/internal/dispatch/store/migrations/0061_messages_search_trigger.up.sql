-- 0061_messages_search_trigger.up.sql
-- LEGION-465 part 6 of 7: messages.search is filled by a trigger, not generated.
--
-- The generated column cannot take the new expression in place on Postgres 16 (SET EXPRESSION is
-- Postgres 17), and dropping and re-adding one rewrites the table under ACCESS EXCLUSIVE for as
-- long as every row's vector takes (29 s per 100 MB of text). DROP EXPRESSION is a catalog change
-- that keeps every value, so the column becomes a plain one that this BEFORE trigger fills on every
-- insert and on every update of the body, exactly as the generated expression did
-- (to_tsvector('english', body)) with search_text applied. No application code writes search. Its
-- own migration, so the transaction holds this table's ACCESS EXCLUSIVE lock alone, for
-- milliseconds, and never while waiting for another table's; 0062 re-indexes the rows the new
-- expression changes.
alter table messages alter column search drop expression;
create function messages_search() returns trigger language plpgsql as $$
begin
  new.search := to_tsvector('english', search_text(new.body));
  return new;
end $$;
create trigger messages_search before insert or update of body on messages
  for each row execute function messages_search();

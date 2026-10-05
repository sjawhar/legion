-- 0075_asks_embeddings_trigger.up.sql
-- Its own migration, so the transaction holds asks' ACCESS EXCLUSIVE lock alone, for
-- milliseconds, and never while waiting for another table's.
--
-- Mirrors search.go's ask leg text (question, then the answer's text) - not the raw options
-- array, which is option labels rather than meaningful prose.
create or replace function embeddings_enqueue_ask() returns trigger language plpgsql as $$
begin
  perform embeddings_enqueue(
    'ask', new.id::text,
    new.question || ' ' || coalesce(new.answer->>'text', '')
  );
  return new;
end;
$$;
create trigger asks_embeddings_enqueue after insert or update of question, answer on asks
  for each row execute function embeddings_enqueue_ask();

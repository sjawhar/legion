-- 0075_comments_embeddings_trigger.up.sql
-- Its own migration, so the transaction holds comments' ACCESS EXCLUSIVE lock alone, for
-- milliseconds, and never while waiting for another table's.
create or replace function embeddings_enqueue_comment() returns trigger language plpgsql as $$
begin
  perform embeddings_enqueue('comment', new.id::text, new.body);
  return new;
end;
$$;
create trigger comments_embeddings_enqueue after insert or update of body on comments
  for each row execute function embeddings_enqueue_comment();

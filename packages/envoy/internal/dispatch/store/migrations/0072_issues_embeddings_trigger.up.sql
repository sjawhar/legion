-- 0072_issues_embeddings_trigger.up.sql
-- Its own migration, so the transaction holds issues' ACCESS EXCLUSIVE lock alone, for
-- milliseconds, and never while waiting for another table's (0057-0061's search triggers made
-- the same choice, for the same reason).
create or replace function embeddings_enqueue_issue() returns trigger language plpgsql as $$
begin
  perform embeddings_enqueue('issue', new.key, new.title);
  return new;
end;
$$;
create trigger issues_embeddings_enqueue after insert or update of title on issues
  for each row execute function embeddings_enqueue_issue();

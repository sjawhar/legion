-- 0076_messages_embeddings_trigger.up.sql
-- Its own migration, so the transaction holds messages' ACCESS EXCLUSIVE lock alone, for
-- milliseconds, and never while waiting for another table's.
--
-- Insert-only, like artifact_versions: nothing in this codebase updates a message's body today.
-- Trigger only on insert, not update, so an editor added later without touching this migration
-- silently never re-embeds an edited message - worth remembering if that changes.
create or replace function embeddings_enqueue_message() returns trigger language plpgsql as $$
begin
  perform embeddings_enqueue('message', new.id::text, new.body);
  return new;
end;
$$;
create trigger messages_embeddings_enqueue after insert on messages
  for each row execute function embeddings_enqueue_message();

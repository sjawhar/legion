create function embeddings_enqueue_message() returns trigger language plpgsql as $$
begin
  perform embeddings_enqueue('message', new.id::text, new.body);
  return new;
end;
$$;
create trigger messages_embeddings_enqueue after insert on messages
  for each row execute function embeddings_enqueue_message();

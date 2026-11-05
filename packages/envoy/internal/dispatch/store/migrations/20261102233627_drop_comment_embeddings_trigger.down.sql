create function embeddings_enqueue_comment() returns trigger language plpgsql as $$
begin
  perform embeddings_enqueue('comment', new.id::text, new.body);
  return new;
end;
$$;
create trigger comments_embeddings_enqueue after insert or update of body on comments
  for each row execute function embeddings_enqueue_comment();

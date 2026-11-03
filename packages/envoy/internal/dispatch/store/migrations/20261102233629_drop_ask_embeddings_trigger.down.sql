create function embeddings_enqueue_ask() returns trigger language plpgsql as $$
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

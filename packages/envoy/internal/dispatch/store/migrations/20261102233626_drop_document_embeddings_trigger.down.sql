create function embeddings_enqueue_document() returns trigger language plpgsql as $$
begin
  if new.markdown is not null then
    perform embeddings_enqueue('document', new.artifact_id::text, new.markdown);
  end if;
  return new;
end;
$$;
create trigger artifact_versions_embeddings_enqueue after insert on artifact_versions
  for each row execute function embeddings_enqueue_document();

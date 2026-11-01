create index asks_session_author_created_at_id_idx
  on asks ((author->>'id'), created_at, id)
  where author->>'kind' = 'session';

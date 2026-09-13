create index events_session_actor_created_at_idx
  on events ((actor->>'id'), created_at desc)
  where actor->>'kind' = 'session';

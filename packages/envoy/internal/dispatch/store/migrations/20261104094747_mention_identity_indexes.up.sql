-- Index the same canonical identities person_aliases and graph_person_edges derive.
-- No existing row is rewritten and no second identity record is stored.
create index comments_session_owner on comments (user_threads_login(author->>'owner'), (author->>'id')) where author->>'kind' = 'session';
create index messages_session_owner on messages (user_threads_login(author->>'owner'), (author->>'id')) where author->>'kind' = 'session';
create index asks_session_owner on asks (user_threads_login(author->>'owner'), (author->>'id')) where author->>'kind' = 'session';
create index events_session_owner on events (user_threads_login(actor->>'owner'), (actor->>'id')) where actor->>'kind' = 'session';
create index comments_person_author on comments (user_threads_login(author->>'id'), created_at desc, id desc) where author->>'kind' = 'user';
create index messages_person_author on messages (user_threads_login(author->>'id'), created_at desc, id desc) where author->>'kind' = 'user';
create index asks_person_author on asks (user_threads_login(author->>'id'), created_at desc, id desc) where author->>'kind' = 'user';
create index comments_created_text_id on comments (created_at desc, (id::text));
create index messages_created_text_id on messages (created_at desc, (id::text));
create index document_blocks_changed_node on document_blocks (changed_at desc, node_id);
create index comment_mentions_comment_id_text on comment_mentions ((comment_id::text));
create index comment_deliveries_comment_id_text on comment_deliveries ((comment_id::text));
create index message_deliveries_message_id_text on message_deliveries ((message_id::text));

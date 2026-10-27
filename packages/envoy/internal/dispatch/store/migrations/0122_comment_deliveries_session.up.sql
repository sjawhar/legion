-- 0122_comment_deliveries_session.up.sql
-- GET /api/v1/agents/{session_id}/answers (store.SessionAnswers) reads one session's comment
-- deliveries - the mentions Dispatch delivered to it - and comment_deliveries has no index on
-- session_id, so without this every read would scan the table. message_deliveries_session (0051)
-- already serves the same read of the session's message deliveries. A delivery whose session is
-- not resolved yet has none and is no session's, so it is left out. A plain `create index`, as
-- every migration's is (the runner applies each inside one transaction, where `concurrently`
-- cannot run): the build holds `SHARE` on comment_deliveries, which blocks writes and not reads,
-- while it reads the table.
create index comment_deliveries_session on comment_deliveries (session_id, created_at desc)
  where session_id is not null;

-- 0147_message_deliveries_session_created.up.sql
-- GET /api/v1/agents/{session_id}/answers (sessionanswers.List) reads one session's sent message
-- deliveries created since an hour before its window. message_deliveries_session (0051) keys
-- session_id alone, so through it that read fetches the session's whole delivery history and
-- filters it by time: a long-lived session that receives every broadcast copy reads tens of
-- thousands of rows on every model request. This index keys time within the session, so the read
-- takes only its window's range, as comment_deliveries_session (0146) does for the session's
-- mentions. It holds only sent deliveries, the read's own predicate, and carries message_id, the
-- one column the read returns, so the read is an index-only scan. That is also what makes a cached
-- generic plan choose it: knowing neither the session nor the window's start, the planner costs a
-- plain (session_id, created_at desc) index within a percent of 0051 and keeps 0051, while an
-- index-only scan costs a fraction of either. message_deliveries_session stays: the conversation
-- window (api/messages.go) reads every delivery a session ever received, at any time, through it.
-- session_id is not null, so no sent row is left out. A plain `create index`, as every migration's
-- is (the runner applies each inside one transaction, where `concurrently` cannot run): the build
-- holds `SHARE` on message_deliveries, which blocks writes and not reads, while it reads the table.
create index message_deliveries_session_created on message_deliveries (session_id, created_at desc)
  include (message_id)
  where state = 'sent';

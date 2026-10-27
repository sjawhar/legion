-- 0121_events_person_words.up.sql
-- GET /api/v1/agents/{session_id}/answers (store.SessionAnswers) reads the events a person wrote of
-- the four types that carry a person's words to a session - an answer, a comment, a message and a
-- reply to a message - created since its window's start (72 hours by default, at most 7 days),
-- and keeps those whose published_destinations hold the session's own topic. Its predicate is the
-- query's own, so the planner reads only the window's human events through this partial index and
-- filters the array on those rows. A GIN index on published_destinations would serve the session
-- directly, but the outbox appends to that array once per destination of every event
-- (outbox.publishDestination), and an index on the column would make every such update write a new
-- index entry instead of staying HOT; this index names neither published_destinations nor any
-- column the outbox updates. It is a plain `create index`: the runner applies each migration inside
-- one transaction, where `concurrently` cannot run, so the build holds `SHARE` on events, which
-- blocks writes and not reads, for as long as it reads the table.
create index events_person_words on events (created_at desc, id desc)
  where actor->>'kind' = 'user'
    and type in ('ask.answered', 'comment.created', 'message.created', 'message.answered');

-- 0082_answers_by_person.up.sql
-- GET /api/v1/me/answers lists one person's answers (ask.answered events they are the actor
-- of) and their replies on asks (ask-thread comments they wrote), newest first. Each index
-- predicate is implied by the query's own, so the planner can use these partial indexes; the
-- query's further check that the actor is the answer's user filters the index's rows, since the
-- planner cannot prove that column-to-column equality from an index predicate.
create index events_ask_answered_by_user
  on events ((actor->>'id'), created_at desc, id desc)
  where type = 'ask.answered' and actor->>'kind' = 'user';
create index comments_ask_replies_by_user
  on comments ((author->>'id'), created_at desc, id desc)
  where ask_id is not null and author->>'kind' = 'user';

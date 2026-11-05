-- 20261105093505_user_agent_thread_dismissed.up.sql
--
-- INFRA-450: the Inbox's Agent rows are one-off messages a person sent an agent session, read
-- straight from messages rather than from a row of their own (api/replies.go, myAgentRootFilter), so a
-- dismissal of one has nowhere to live. This table is that mark: one row per person per dismissed
-- root message, keyed on the login the Inbox reads by (model.CanonicalLogin, as user_agent_read and
-- user_agent_state are keyed) and the root message's id.
--
-- A later reply from the session brings the thread back: 20261105093507 clears the mark in that
-- reply's own transaction, as user_threads_record clears a Reply thread's, so a mark existing is
-- what "dismissed" means here and no read compares two times (api/replies.go,
-- myAgentDismissedRoot). A dismiss of a thread that came back writes its row again, the one the
-- trigger deleted.
--
-- The index is the Dismissed view's read: the login it narrows to, then the order it answers in and
-- the cursor it resumes from (newest dismissal first, ties by message id descending), so one index
-- scan answers a page of 200 with no sort. The primary key is what the Inbox list's own read
-- asks - is this root dismissed for this login - and what a dismiss and a restore write by key.
--
-- A create table and its index take ACCESS EXCLUSIVE on a relation no other transaction can see
-- yet, since the table does not exist until this migration commits: no reader and no writer of any
-- existing table waits, nothing is backfilled, and the census answers 0. There is no foreign key to
-- messages, as user_threads has none to the threads it names: nothing deletes a message, and a
-- harness that empties messages empties this table with them.
--
-- Measured on this pod's test database: user_threads, the table the Inbox's other half reads, held
-- 24 rows after the api suite's Inbox seeds, the largest any one test's database held while
-- `go test ./internal/dispatch/api/ -run 'InboxRead|Replies|InboxRows'` ran (sampled every 0.3 s).
-- This table is created empty whatever that count is, here and in production.
create table user_agent_thread_dismissed (
  login        text not null,
  message_id   uuid not null,
  dismissed_at timestamptz not null,
  primary key (login, message_id)
);

create index user_agent_thread_dismissed_login_dismissed_at
  on user_agent_thread_dismissed (login, dismissed_at desc, message_id desc);

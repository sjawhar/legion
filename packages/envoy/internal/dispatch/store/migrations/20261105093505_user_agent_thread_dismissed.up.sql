-- 20261105093505_user_agent_thread_dismissed.up.sql
--
-- INFRA-450: the Inbox's Agent rows are one-off messages a person sent an agent session, read
-- straight from messages rather than from a row of their own (api/replies_agent.go,
-- myAgentRootFilter), so a dismissal of one has nowhere to live. This table is that mark: one row
-- per person per dismissed root message, keyed on the login the Inbox reads by
-- (model.CanonicalLogin, as user_agent_read and user_agent_state are keyed) and the root
-- message's id.
--
-- A later reply from the session brings the thread back: 20261105093507 clears the mark in that
-- reply's own transaction, as user_threads_record clears a Reply thread's, so a mark existing is
-- what "dismissed" means here and no read compares two times (api/replies_agent.go,
-- myAgentDismissedRoot). A dismiss of a thread that came back writes its row again, the one the
-- trigger deleted.
--
-- The key is (message_id, login) rather than (login, message_id), which is the one order that
-- leaves this table two indexes instead of three. Three reads ask it anything at all. A dismiss
-- and a restore name both columns, so either order answers them. The Inbox's own reads ask for one
-- person's whole set of marks (api/replies_agent.go, myAgentDismissedRootSet), which the index below
-- answers index-only, login first. And 20261105093507's trigger deletes by the root alone - the
-- reply that brings a thread back does not name whoever dismissed it - which only an index with
-- message_id leading can read: under the other order every session reply would scan the table,
-- one row per person per thread they have ever dismissed, 110,000 of them for one person at
-- INFRA-450's a14 seed.
--
-- The index is the Dismissed view's read: the login it narrows to, then the order it answers in
-- and the cursor it resumes from (newest dismissal first, ties by message id descending), so one
-- index scan answers a page of 200 with no sort.
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
  primary key (message_id, login)
);

create index user_agent_thread_dismissed_login_dismissed_at
  on user_agent_thread_dismissed (login, dismissed_at desc, message_id desc);

-- 0048_user_ask_snooze.up.sql
-- A human's Snooze on one Inbox row: the ask is still open and still listed, but this viewer
-- has said they will deal with it after snoozed_until. The Inbox surfaces the timestamp and
-- folds the row into its Later section while it is in the future; nothing sweeps the table,
-- because a row whose moment has passed is an ordinary Inbox row again on the next read.
--
-- Keyed on the ask rather than its issue, so a document ask - which no issue owns, and which
-- user_issue_state therefore cannot hold - snoozes exactly like an issue ask. Per (login, ask),
-- so the snooze follows the human across devices the way user_agent_state does for agents and
-- takes nobody else's row out of their inbox.
create table user_ask_snooze (
  login text not null,
  ask_id uuid not null references asks(id) on delete cascade,
  snoozed_until timestamptz not null,
  primary key (login, ask_id)
);

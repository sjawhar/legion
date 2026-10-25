-- 0116_agent_message_idempotency_keys.up.sql
-- One row per single-session send a person made, keyed by the idempotency key the send carried
-- (INFRA-258): a repeat of POST /api/v1/agents/{session_id}/messages with a key this person has
-- already used is answered the message that key made, instead of a second message that hands the
-- session the same text twice. The page retries a send under its key after a 5xx, a 429 or a
-- request that timed out, none of which says whether the first one committed. login is
-- model.CanonicalLogin, the form 0055's broadcast_idempotency_keys and the other per-person tables
-- key on, so nobody can replay or collide with another person's send. request_digest is a SHA-256
-- of what the send asked for - its session, body, mode and in_reply_to, after validation - so a
-- key reused for a different request is refused rather than answered with a message that says
-- something else.
--
-- A key is recognised for as long as its message exists. It is a row, not a stream entry, so
-- unlike the delivery stream's DELIVERY_DUPLICATE_WINDOW_MS there is no window and nothing to
-- sweep. Messages from before this migration carried no key and have no row; nothing is
-- backfilled.
--
-- A table of its own rather than columns and a unique index on messages, for 0055's reason: the
-- runner applies a migration in one transaction, so an index on an existing table would hold its
-- writes for the length of the build, where an empty table's primary key costs nothing. The one
-- lock this takes on an existing table is the foreign key's SHARE ROW EXCLUSIVE on messages for
-- this statement: it scans no row and blocks a message write only for the instant the transaction
-- lasts, and pgmigrate.LockTimeout bounds the wait for it. The release before this one neither
-- reads nor writes the table, which is what a rolling update requires of a migration
-- (cmd/dispatch/README.md, "Database migrations").
create table agent_message_idempotency_keys (
  login text not null,
  idempotency_key text not null,
  message_id uuid not null references messages (id),
  request_digest text not null,
  created_at timestamptz not null default clock_timestamp(),
  primary key (login, idempotency_key)
);

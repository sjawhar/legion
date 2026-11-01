-- 0055_broadcast_idempotency_keys.up.sql
-- One row per broadcast a human sent, keyed by the idempotency key the send carried
-- (LEGION-446): a repeat of POST /api/v1/broadcasts with a key this human has already used is
-- answered the broadcast that key made, instead of a second broadcast that hands every
-- recipient the same message twice. login is canonicalLogin, the form the per-person tables
-- (user_ask_snooze and the rest) key on, so nobody can replay or collide with another person's
-- send. request_digest is a SHA-256 of what the send asked for - body, mode and the requested
-- sessions in order, after validation - so a key reused for a different request is refused
-- rather than answered with a broadcast that says something else.
--
-- A key is recognised for as long as its broadcast exists. It is a row, not a stream entry, so
-- unlike the delivery stream's DELIVERY_DUPLICATE_WINDOW_MS there is no window and nothing to
-- sweep. Broadcasts from before this migration carried no key and have no row; nothing is
-- backfilled.
--
-- A table of its own rather than columns and a unique index on broadcasts: the runner applies a
-- migration in one transaction (0052 says what that costs), so an index on an existing table
-- would hold its writes for the length of the build, where an empty table's primary key costs
-- nothing. The one lock this takes on an existing table is the foreign key's SHARE ROW EXCLUSIVE
-- on broadcasts for this statement: it scans no row and blocks only a broadcast create for the
-- instant the transaction lasts, and pgmigrate.LockTimeout bounds the wait for it.
create table broadcast_idempotency_keys (
  login text not null,
  idempotency_key text not null,
  broadcast_id uuid not null references broadcasts(id),
  request_digest text not null,
  primary key (login, idempotency_key)
);

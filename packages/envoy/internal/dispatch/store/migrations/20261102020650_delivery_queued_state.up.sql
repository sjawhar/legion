-- 20261102020650_delivery_queued_state.up.sql
-- INFRA-626: a fourth delivery attempt state, `queued`. The Envoy listener now takes a message for
-- a session it knows and that is not running, stores it, and answers `state: "stopped"`
-- (cmd/listener/api.go, sendHandler); the session reads it from its own consumer when it runs
-- again, within the 72 hours the stream keeps it. Dispatch records such an attempt `queued` rather
-- than `failed`, so a person's direct message, mention or broadcast to a stopped but known session
-- waits for that session instead of being thrown away.
--
-- Every statement widens what the schema allows and narrows nothing, which is what a rolling
-- update requires (packages/envoy/cmd/dispatch/README.md, "Database migrations"): the release
-- before this one writes only 'pending', 'sent' and 'failed', and reads only 'sent', both of which
-- this schema still holds, so the old build and the new build run against it at once.
--
-- The two state checks, 0039's on comment_deliveries and 0046's on message_deliveries, gain
-- 'queued'. Each is added without `not valid`, so Postgres reads the table once to validate it:
-- no stored row can fail a check wider than the one it was written under, and an unvalidated
-- constraint would be carried for nothing.
--
-- message_deliveries_session_created (0147) backs the message-delivery arm of
-- GET /api/v1/agents/{session_id}/answers (sessionanswers.query) and held `state = 'sent'` rows
-- alone. A queued attempt reached its session as a sent one did, so that read must see those rows
-- too: the index is rebuilt over both states. It carries `state` beside `message_id` now, because
-- Postgres drops an index predicate's clause from a scan only for a predicate it can prove, and a
-- predicate over two states keeps the read's own `state in ('sent', 'queued')` test; a column the
-- index does not carry would then turn the index-only scan 0147 exists for into a heap read of
-- every row it found. One more text column per row buys that scan back.
--
-- message_deliveries gains `connected_only`: whether the send that opened the attempt asked for a
-- session that is running now, which the Agents page's model and effort switch alone asks
-- (api/messages.go, createMessageInput.ConnectedOnly). It is persisted because a later send of
-- that attempt is made by someone else - the delivery sweep, or a repeat under the request's key -
-- and a switch resumed without the flag would be stored for a stopped session and applied at an
-- unknown later time, which this issue's non-goals forbid. It is also what a later attempt of the
-- same message is opened with, read off the message's first attempt
-- (api/message_delivery.go, insertMessageDeliveryAttempt), so a person's Retry or mode change of
-- a refused switch is refused again rather than stored. comment_deliveries gains nothing: a
-- comment's mention is never a switch, and no mention asks for a connected session. `not null
-- default false` reads as every stored row's own answer, and a non-volatile default is written
-- into the catalogue alone in Postgres 11 and later, so no row is rewritten.
--
-- message_deliveries_pending and comment_deliveries_pending (0138) stay as they are. They back the
-- delivery sweep's read of attempts still owed a send (api/delivery_sweep.go, owedMessageAttempt
-- and owedCommentAttempt), and a queued attempt is settled - the listener took it - so the sweep
-- must never pick one up and the predicate `state = 'pending'` is exactly right.
--
-- No row is refused or rewritten, so the census answers 0. Each check, the new column and the
-- index rebuild takes ACCESS EXCLUSIVE on its own table for as long as one read of it takes, and
-- the column for no longer than its catalogue write. Rolling back past this
-- migration is an image rollback alone while no row carries 'queued'; once one does, the checks
-- have to be widened again by hand before an earlier set is restored.
alter table comment_deliveries drop constraint comment_deliveries_state_check;
alter table comment_deliveries add constraint comment_deliveries_state_check
  check (state in ('pending', 'sent', 'queued', 'failed'));

alter table message_deliveries drop constraint message_deliveries_state_check;
alter table message_deliveries add constraint message_deliveries_state_check
  check (state in ('pending', 'sent', 'queued', 'failed'));

drop index message_deliveries_session_created;
create index message_deliveries_session_created on message_deliveries (session_id, created_at desc)
  include (message_id, state)
  where state in ('sent', 'queued');

alter table message_deliveries add column connected_only boolean not null default false;

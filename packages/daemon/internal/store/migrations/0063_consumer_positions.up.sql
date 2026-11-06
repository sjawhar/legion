-- 0063_consumer_positions.up.sql — each durable consumer's position: the stream sequence and
-- stream time of the last message it applied, written in that message's fact transaction
-- (intake.ApplyFact), advancing only.
--
-- A durable consumer found missing (deleted, or lost with the NATS server's state) is created
-- delivering from stream_time, so the events published while it was missing are applied rather
-- than skipped; the message at stream_time itself is replayed and dropped as a duplicate through
-- processed_events. The start is the time, not the sequence: a stream that is recreated restarts
-- its sequences at 1, and a stored sequence would then skip every message until the new stream
-- reached it. stream_seq is kept for the operator reading this table.
create table consumer_positions (
  consumer text primary key,
  stream_seq bigint not null,
  stream_time timestamptz not null
);

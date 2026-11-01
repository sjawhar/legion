-- A delivery attempt is committed before its Envoy listener send and settled after it, so
-- neither send runs with a transaction or a pooled connection held (LEGION-244).

-- claimed_at is when a sender took a pending attempt and started sending it. A retry resumes an
-- attempt nobody holds - never claimed, or claimed by a process that died between the commit and
-- the outcome - under its original number, and so its original idempotency key, which the
-- listener deduplicates against a send that did land; it takes a fresh attempt number only while
-- a live sender holds the pending one.
alter table comment_deliveries add column claimed_at timestamptz;

-- A message attempt is now recorded as pending before its send, exactly like a comment's, and
-- claimed the same way.
alter table message_deliveries add column claimed_at timestamptz;
alter table message_deliveries drop constraint message_deliveries_state_check;
alter table message_deliveries add constraint message_deliveries_state_check
  check (state in ('pending', 'sent', 'failed'));

-- 0149_message_delivery_urgency.up.sql
-- The urgency a targeted message was sent with, kept on its attempt row so a send that resumes a
-- stranded attempt (api/delivery_sweep.go, resumeOwedDeliveries) hands the listener the urgency
-- the person chose rather than none. A direct message's attempt 1 is written in the transaction
-- that stores the message, with the request's urgency; a broadcast recipient's carries none, as a
-- broadcast has none. It is internal: no API read selects it. Nullable with no default, so the
-- release before this one, which names no urgency in its inserts, rolls beside it, and a resumed
-- attempt such a release wrote goes out without an urgency, as its first send did. The column and
-- its check are two statements: an inline check is validated at once, which reads every row of
-- the table under its ACCESS EXCLUSIVE lock, while `not valid` writes the catalog alone and still
-- refuses every row written or updated after it (packages/envoy/cmd/dispatch/README.md). No
-- release validates it: every row below it carries no urgency at all.
alter table message_deliveries add column urgency text;
alter table message_deliveries add constraint message_deliveries_urgency_check
  check (urgency in ('low', 'med', 'high', 'blocking')) not valid;

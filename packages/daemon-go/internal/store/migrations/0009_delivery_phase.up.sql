-- 0009_delivery_phase.up.sql — the phase a pending task was queued for.
--
-- A task the workflow queues is for the phase its issue was in; once the issue is in another
-- phase the task is finished work and is dropped rather than sent. The phase travels with the
-- delivery because nothing else on it survives every path a delivery takes: its id is rotated by
-- a prompt retry. A delivery of no phase — an operator's own, an architect's — is held whatever
-- phase its issue is in, and is written as the empty string.
alter table pending_task_deliveries add column phase text not null default '';

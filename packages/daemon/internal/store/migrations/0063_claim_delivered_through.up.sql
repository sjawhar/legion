-- 0063_claim_delivered_through.up.sql — the newest outbox start a claim queued a task from.
--
-- A start row delivers its task to the claim (supervise's queue) and then finishes; a failure or
-- a crash between the two runs the row again. The claim deduplicated only against a delivery still
-- pending, so a retry after the architect's turn on the task had ended and retired it queued the
-- whole task again, and the architect was told each decision in it twice. The claim records the
-- row id of the newest start whose task it queued, in the transaction that writes that delivery,
-- so a retry of that row, or of an older one, queues nothing whether its delivery is pending or
-- retired. Zero is a claim no row has given a task.
alter table claims add column delivered_through bigint not null default 0;

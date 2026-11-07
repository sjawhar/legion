-- 20261107041812_delivery_send_failures.up.sql — the record of the sends of a task that failed.
--
-- pending_task_deliveries.send_failures, last_send_failure_at and last_send_error are
-- supervise.Delivery.SendFailures, LastSendFailureAt and LastSendError: how many sends of this
-- pending task did not reach a turn, when the last of them failed and the error it failed with.
-- They are a diagnosis record rather than a budget — the budget is the claim's prompt_failures
-- and prompt_retires, which only a charged failure spends, while these count every failed send,
-- charged or not — and the operator's claim view serves them beside the queued, delivered and
-- confirmed times (api.DeliveryView), so a claim waiting on a send that never lands says so
-- without a person opening a shell in its pod (INFRA-1480).
--
-- Zero, null and empty for every existing delivery, which carries no such record, and each column
-- has a default because the previous release's daemon writes this table without naming them. That
-- is also what an image roll-back leaves: the older upsert names none of the three, so whatever
-- the newer binary last wrote freezes there until a new delivery row replaces it, and a confirmed
-- send no longer clears it — an operator reading the view then sees a failure count that no later
-- success cleared. Rolling the image back is therefore a roll-back of the image alone, as the
-- note at internal/store/claims.go says of the rest of this table.
alter table pending_task_deliveries add column send_failures integer not null default 0;
alter table pending_task_deliveries add column last_send_failure_at timestamptz;
alter table pending_task_deliveries add column last_send_error text not null default '';

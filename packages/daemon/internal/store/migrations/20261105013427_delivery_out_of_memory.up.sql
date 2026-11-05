-- 20261105013427_delivery_out_of_memory.up.sql — the task whose turn its pod ran out of memory.
--
-- pending_task_deliveries.out_of_memory is supervise.Delivery.OutOfMemory: the end that cut this
-- task's turn off was the kubelet ending the pod's one container for running out of memory, so the
-- task is re-sent behind the note saying so and pointing at what not to run in a pod. False for
-- every existing delivery, which carries no such end, and the column has a default because the
-- previous release's daemon writes this table without it (the note at internal/store/claims.go
-- records why a rollback has to stay a rollback of the image alone).
alter table pending_task_deliveries add column out_of_memory boolean not null default false;

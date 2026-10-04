-- 0032_issue_suspend.up.sql — an issue's close suspends its Sandbox in a durable effect of its own,
-- apart from the per-role stops before it and the tree cleanup after it.
alter table outbox drop constraint outbox_kind_check;
alter table outbox add constraint outbox_kind_check check (kind in (
  'dispatch_status', 'dispatch_message', 'notice', 'controller_notice', 'supervise', 'gate_seed',
  'linger_close', 'workspace_remove', 'merge_queue_publish', 'issue_suspend'));

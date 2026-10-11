-- 0038_auto_merge_disable_kind.up.sql — the outbox kind that disables a pull request's auto-merge,
-- and takes it out of the merge queue, when a push moves it past its READY head or the READY is
-- withdrawn (auto_merge_disable), beside every kind 0034 admits.
alter table outbox drop constraint outbox_kind_check;
alter table outbox add constraint outbox_kind_check check (kind in (
  'dispatch_status', 'dispatch_message', 'notice', 'controller_notice', 'supervise', 'gate_seed',
  'linger_close', 'workspace_remove', 'merge_queue_publish', 'issue_branch', 'issue_suspend',
  'auto_merge_disable'));

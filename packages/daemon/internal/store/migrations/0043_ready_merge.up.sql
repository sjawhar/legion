-- 0043_ready_merge.up.sql — the outbox kind that submits an accepted READY's merge at the head it
-- named (ready_merge), beside every kind 0039 admits. The check is rewritten whole, naming every
-- kind: this sets the list after every migration that adds a kind.
alter table outbox drop constraint outbox_kind_check;
alter table outbox add constraint outbox_kind_check check (kind in (
  'dispatch_status', 'dispatch_message', 'notice', 'controller_notice', 'supervise', 'gate_seed',
  'linger_close', 'workspace_remove', 'merge_queue_publish', 'issue_branch', 'issue_suspend',
  'auto_merge_disable', 'ready_merge'));

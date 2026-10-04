-- 0029_issue_branch_kind.up.sql — the outbox kind that creates an issue's branch on GitHub before
-- any role of the issue starts (issue_branch), beside every kind 0021 admits.
--
-- The check is one list, so the migration that sets it names every kind.
alter table outbox drop constraint outbox_kind_check;
alter table outbox add constraint outbox_kind_check check (kind in (
  'dispatch_status', 'dispatch_message', 'notice', 'controller_notice', 'supervise', 'gate_seed',
  'linger_close', 'workspace_remove', 'merge_queue_publish', 'issue_branch'));

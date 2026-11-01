-- 20261029120600_role_message.up.sql — the role message's outbox kind. A CI settlement moves no issue
-- (dispatch://INFRA-714's R1); what it names is told to the roles that work the issue - the
-- worker holding the phase, the implementer and the tree's architect - as one message per role
-- published to that role's topic (record.RoleMessage, workflow's tellChecks, daemon's
-- roleMessage), which is a durable row so the telling survives a restart and carries the outbox's
-- own retry budget.
--
-- The check is rewritten whole, naming every kind: this sets the list after every migration that
-- adds a kind.
alter table outbox drop constraint outbox_kind_check;
alter table outbox add constraint outbox_kind_check check (kind in (
  'dispatch_status', 'dispatch_message', 'notice', 'controller_notice', 'supervise', 'gate_seed',
  'linger_close', 'workspace_remove', 'merge_queue_publish', 'issue_branch', 'issue_suspend',
  'auto_merge_disable', 'ready_merge', 'check_compare', 'check_look', 'check_bound',
  'approval_carry', 'role_message'));

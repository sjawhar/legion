-- 0074_approval_carry.up.sql — the approval carry's outbox kind. With no periodic pass left to
-- read GitHub (dispatch://INFRA-714's R1), the compare that carries a reviewer's approval across a
-- handoff-only push is driven by the push event itself, as a durable row with the outbox's own
-- retry and rate-limit budget (record.ApprovalCarry, daemon's approvalCarry): a single synchronous
-- compare in the push handler has no retry, and one rate-limited answer would silently drop an
-- approval.
--
-- The check is rewritten whole, naming every kind: this sets the list after every migration that
-- adds a kind.
alter table outbox drop constraint outbox_kind_check;
alter table outbox add constraint outbox_kind_check check (kind in (
  'dispatch_status', 'dispatch_message', 'notice', 'controller_notice', 'supervise', 'gate_seed',
  'linger_close', 'workspace_remove', 'merge_queue_publish', 'issue_branch', 'issue_suspend',
  'auto_merge_disable', 'ready_merge', 'check_compare', 'check_look', 'check_bound',
  'approval_carry'));

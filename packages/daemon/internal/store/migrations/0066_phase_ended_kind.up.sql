-- 0066_phase_ended_kind.up.sql — the outbox takes the `phase_ended` kind, the message the daemon
-- publishes to the Envoy topic of the role whose phase a CI-red transition ended: the tester, the
-- reviewer, the merger, or the implementer in retro. Without the start takeover nothing else tells
-- that role, since the daemon's next task for a role waits for that role's turn to end. The
-- constraint is rewritten whole, naming every kind: this sets the list after every migration that
-- adds a kind, as 0049 did.
alter table outbox drop constraint outbox_kind_check;
alter table outbox add constraint outbox_kind_check check (kind in (
  'dispatch_status', 'dispatch_message', 'notice', 'controller_notice', 'supervise', 'gate_seed',
  'linger_close', 'workspace_remove', 'merge_queue_publish', 'issue_branch', 'issue_suspend',
  'auto_merge_disable', 'ready_merge', 'check_compare', 'check_look', 'check_bound',
  'phase_ended'));

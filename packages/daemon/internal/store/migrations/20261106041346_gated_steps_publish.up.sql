-- 20261106041346_gated_steps_publish.up.sql — the outbox kind that publishes a deployment's
-- design-gate steps onto its Dispatch project row (gated_steps_publish).
--
-- The daemon publishes the list it is configured with at every boot, so a person reading Dispatch
-- sees what the deployment holds a tree's work for. The publish rides the outbox like every other
-- Dispatch write, so a Dispatch that is down at boot does not fail the boot and the row is
-- retried until it lands. Its row belongs to no issue: the issue column holds the bare project
-- key, which is what split_part(issue, '-', 1) already answers for every reader that scopes a row
-- to its own daemon, since a project key holds no hyphen. The controller's tick row is addressed
-- the same way.
--
-- The check is one list, so the migration that sets it names every kind, as 0030 and
-- 20261101222956 did: whichever of two open branches merges last admits both their kinds.
alter table outbox drop constraint outbox_kind_check;
alter table outbox add constraint outbox_kind_check check (kind in (
  'dispatch_status', 'dispatch_message', 'notice', 'controller_notice', 'supervise', 'gate_seed',
  'linger_close', 'workspace_remove', 'merge_queue_publish', 'issue_branch', 'issue_suspend',
  'auto_merge_disable', 'ready_merge', 'check_compare', 'check_look', 'check_bound',
  'phase_ended', 'issue_bundle', 'issue_release', 'gated_steps_publish'));

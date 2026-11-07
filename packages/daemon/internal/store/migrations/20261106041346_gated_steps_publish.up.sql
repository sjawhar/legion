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
  'auto_merge_disable', 'ready_merge', 'check_wake', 'check_look', 'check_bound',
  'phase_ended', 'issue_bundle', 'issue_release', 'gated_steps_publish'));

-- One publish row per project, which is what lets the enqueue be an upsert
-- (record.Postgres.EnqueueGatedSteps): the row waits on its lane until Dispatch takes it, and a
-- boot inside that window replaces the list it carries rather than queueing beside it. The
-- index holds gated_steps_publish rows alone, so no other kind's insert conflicts on it, and
-- the kind is new in this migration, so there is nothing to collapse first.
create unique index outbox_gated_steps_publish_project_unique
  on outbox (issue)
  where kind = 'gated_steps_publish';

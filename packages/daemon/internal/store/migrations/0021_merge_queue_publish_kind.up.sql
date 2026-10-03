-- 0021_merge_queue_publish_kind.up.sql — the outbox kind of the merger's READY published to the
-- project's merge queue role (merge_queue_publish), beside every kind 0020 admits.
--
-- The check is one list, so the migration that sets it names every kind. It runs after 0020, which
-- added controller_notice; a database already past 0020 applies the lower 0016 late, which is why
-- 0016 leaves the check alone.
alter table outbox drop constraint outbox_kind_check;
alter table outbox add constraint outbox_kind_check check (kind in (
  'dispatch_status', 'dispatch_message', 'notice', 'controller_notice', 'supervise', 'gate_seed',
  'linger_close', 'workspace_remove', 'merge_queue_publish'));

-- 20261107095842_outbox_kinds_without_the_merge_lane.up.sql — the outbox's kinds once the daemon
-- merges nothing (dispatch://INFRA-714 R2) and the production check names its own wake
-- (INFRA-809).
--
-- Two releases wrote this constraint and neither names the set the daemon now writes.
-- 20261105081500 named this release's kinds with the idle release's two, and it still allowed
-- `merge_queue_publish`, `auto_merge_disable` and `ready_merge` nowhere — it already dropped them
-- — but named `check_compare`, which INFRA-809 renamed. 20261105231916 then named `check_wake`
-- and brought the three merge-lane kinds back, since it was written on a branch where the daemon
-- still merged, and it left `role_message` out, since that kind is this release's own. A database
-- ending at either one therefore refuses a row the daemon writes, or allows a row nothing writes.
--
-- This migration is the last word of both, as 20261105081500 was of the two before it: it names
-- the kinds the merged daemon writes and nothing else — this release's seventeen, with
-- `check_wake` for the compare INFRA-809 retired. It is safe on a database ending at either
-- earlier version, and on one that already carries a constraint of this name from any of them,
-- because it drops the constraint if it exists before adding it. No row is deleted: a row of a
-- retired kind exists in no database, both releases having stopped writing them before this.
alter table outbox drop constraint if exists outbox_kind_check;
alter table outbox add constraint outbox_kind_check check (kind in (
  'dispatch_status', 'dispatch_message', 'notice', 'controller_notice', 'supervise', 'gate_seed',
  'linger_close', 'workspace_remove', 'issue_branch', 'issue_suspend', 'check_wake',
  'check_look', 'check_bound', 'role_message', 'phase_ended', 'issue_bundle', 'issue_release'));

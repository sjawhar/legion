-- 20261105081500_outbox_kinds_of_both_releases.up.sql — one outbox_kind_check holding the kinds
-- of this release and of the idle release it merged (dispatch://INFRA-714).
--
-- Two releases rewrote this constraint from bases that did not know each other. This release's
-- 20261029120800 dropped the three kinds of the daemon's own merge and added `role_message`;
-- main's 20261101222956 added `issue_bundle` and `issue_release` to the list as it stood before
-- that drop. Which of the two a database ends at is decided by what it had already recorded, not
-- by the file order: Store.apply asks whether each migration's own version is recorded, so a
-- database that took the idle release first skips 20261101222956 for ever and ends at this
-- release's list, which names neither of the idle release's kinds, and the daemon's own
-- `issue_bundle` and `issue_release` rows are then refused by the constraint (internal/daemon's
-- idle release queues both on the lifecycle lane). A fresh database applies them the other way
-- round and ends at the idle release's list, which names no `role_message`.
--
-- So neither file can be the last word, and editing either reaches only the databases that have
-- not recorded it. This migration is a version both orders lack: it names the union of what the
-- two releases left — this release's fifteen kinds with the idle release's two — and every
-- database ends at it whichever order ran. It is written to be safe to run on either shape, and
-- on one that already carries a constraint of this name from any of them, by dropping the
-- constraint if it exists before adding it.
--
-- It names `check_wake` beside `check_compare`: INFRA-809 renamed the compare kind on main, and a
-- database that ran main's own rewrite has rows of the new name, which this list has to admit
-- when it runs late over them. The compare kind stays named for the database that queued one
-- under the release before that rename. 20261107095842 is the last word of both lists and names
-- only what the merged daemon writes.
alter table outbox drop constraint if exists outbox_kind_check;
alter table outbox add constraint outbox_kind_check check (kind in (
  'dispatch_status', 'dispatch_message', 'notice', 'controller_notice', 'supervise', 'gate_seed',
  'linger_close', 'workspace_remove', 'issue_branch', 'issue_suspend', 'check_compare',
  'check_wake', 'check_look', 'check_bound', 'role_message', 'phase_ended', 'issue_bundle',
  'issue_release'));

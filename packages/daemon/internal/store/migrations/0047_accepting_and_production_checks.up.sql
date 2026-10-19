-- 0047_accepting_and_production_checks.up.sql — the architect accepts before the merge, the merge
-- closes the issue, and the production check becomes work after the close (LEGION-781).
--
-- Phases. `accepting` sits between `reviewing` and `retro`; `production_check` is no phase. An
-- issue the old daemon left in production_check has its pull request merged (that phase began at
-- the merge), so it stands where the new daemon closes it: awaiting_merge, whose merged pull
-- request the boot's sweep closes. The same for a hold taken from production_check. The move runs
-- before the constraints are swapped, since the new lists refuse the old value.
update issues set phase = 'awaiting_merge' where phase = 'production_check';
update issues set held_from = 'awaiting_merge' where held_from = 'production_check';

alter table issues drop constraint issues_phase_check;
alter table issues add constraint issues_phase_check check (phase in (
  'admitted', 'planning', 'implementing', 'testing', 'reviewing', 'accepting', 'retro', 'merging',
  'awaiting_merge', 'done', 'held'));
alter table issues drop constraint issues_held_from_check;
alter table issues add constraint issues_held_from_check check (held_from in (
  'admitted', 'planning', 'implementing', 'testing', 'reviewing', 'accepting', 'retro', 'merging',
  'awaiting_merge', 'done'));

-- The merge commit. The pull request keeps the commit GitHub reported at its merge, empty until
-- then and for a pull request merged before this column existed: the check the close opens copies
-- it, since the check outlives the pull request's row (below).
alter table pull_requests add column merge_sha text not null default '';

-- The production check, one per closed issue, fenced on itself rather than on the tree generation:
-- it copies the repository, pull request number and merge commit, since ClearTreeGeneration
-- deletes a merged pull_requests row at re-admission and leaves this table alone. looks counts the
-- implementer's looks; look_open is whether the latest is still running, and look_wake_at when it
-- woke. last_wake_* is what woke the latest look and pending_cause_* a wake that arrived while a
-- look was open, which the next look starts with. last marks the bound's look, the last one. A
-- closed check has closed_at and its outcome: recorded (pass), defect (fail) or not_run (pending
-- at the last look, or a look that could not run).
create table production_checks (
  issue text primary key references issues (key) on delete cascade,
  project text not null,
  tree text not null,
  repo text not null,
  pr_number integer not null,
  merge_sha text not null default '',
  opened_at timestamptz not null,
  bound_at timestamptz not null,
  looks integer not null default 0,
  look_open boolean not null default false,
  look_wake_at timestamptz,
  last_wake_kind text not null default '',
  last_wake_detail text not null default '',
  last_wake_at timestamptz,
  pending_cause_kind text not null default '',
  pending_cause_detail text not null default '',
  last boolean not null default false,
  closed_at timestamptz,
  outcome text check (outcome in ('recorded', 'defect', 'not_run')));
-- The open checks of a tree: what a lingering root's expiry waits for, and the boot's wake reads.
create index production_checks_open_tree on production_checks (tree) where closed_at is null;

-- The check's outbox kinds, beside every kind 0043 admits: check_compare (a GitHub compare of the
-- check's merge commit with a build commit, waking the check when the build contains the merge),
-- check_look (a look's bound) and check_bound (the whole check's bound). The check is rewritten
-- whole, naming every kind: this sets the list after every migration that adds a kind.
alter table outbox drop constraint outbox_kind_check;
alter table outbox add constraint outbox_kind_check check (kind in (
  'dispatch_status', 'dispatch_message', 'notice', 'controller_notice', 'supervise', 'gate_seed',
  'linger_close', 'workspace_remove', 'merge_queue_publish', 'issue_branch', 'issue_suspend',
  'auto_merge_disable', 'ready_merge', 'check_compare', 'check_look', 'check_bound'));

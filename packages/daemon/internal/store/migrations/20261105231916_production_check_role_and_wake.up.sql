-- 20261105231916_production_check_role_and_wake.up.sql — the production check names the role that
-- works it and the time it is next looked at, the claim and its delivery name the look the worker
-- took, and the compare kind goes (INFRA-809).
--
-- This migration is this branch's own and is not released, so every column INFRA-809 needs is
-- added here rather than in a second migration: there is no deployed store that has run it.
--
-- role is the role that works the check. It is written once, when the check opens, from the
-- project's `production_check` setting (`off` opens no check, `tester` opens one the tester
-- works), and never read from the configuration again, so a setting changed while a check is open
-- leaves that check with the role it started under.
-- The default is `implementer` because the table already holds rows: a column that is not null
-- needs one for them, and `implementer` is the role every check open at this release is already
-- being worked by, so each of those rows keeps the role it is being worked under.
--
-- This migration is one way. After it a daemon of the earlier release fails every
-- production_checks read and write, because its statements name pending_cause_kind and
-- pending_cause_detail, which this migration drops; going back therefore needs the database
-- restored from a backup. Several projects' daemons can share one database
-- (`docs/kubernetes.md`), so those daemons move to this release together.
alter table production_checks add column role text not null default 'implementer';

-- wake_at is when the check is next looked at: the time the checking agent named for itself on a
-- pending report, or the retry of a look nobody reported. Null where neither stands, which is
-- every check open at this release: none has named a wake, since until now nothing could.
alter table production_checks add column wake_at timestamptz;

-- pending_cause_kind and pending_cause_detail held a wake that arrived while a look was open,
-- which the next look started with. Their one writer was the daemon's wake, which guessed from a
-- build or a deploy event that production carried the merge; that is deleted, so nothing writes
-- them and the branch that read them is dead.
alter table production_checks drop column pending_cause_kind;
alter table production_checks drop column pending_cause_detail;

-- check_compare was that guess as an outbox row: a GitHub compare of the check's merge commit
-- with a build, or a deploy run read for the same purpose. Its executor is gone, so a row still
-- queued is one no daemon can run. The rows are deleted before the constraint below is rewritten,
-- because Postgres validates a new CHECK against the rows already in the table and a store
-- holding one queued check_compare row would otherwise refuse this release's boot. Deleting rows
-- a migration leaves unrunnable is what 0016_ready_packet.up.sql:19,25,
-- 0035_resident_roles.up.sql:9 and 0044_ready_merge_dedup.up.sql did.
delete from outbox where kind = 'check_compare';

-- check_wake takes its place: the one row that can start a check's next look beside the check's
-- own bound (check_bound) — the wake the checking agent named, or the retry of a look nobody
-- reported. The constraint is rewritten whole, naming every kind, as 0049:61-65, 0066:7-12 and
-- 20261101222956_issue_idle_release:11-15 did: this sets the list after every migration that adds
-- a kind, and a kind left out would stop its own effect.
alter table outbox drop constraint outbox_kind_check;
alter table outbox add constraint outbox_kind_check check (kind in (
  'dispatch_status', 'dispatch_message', 'notice', 'controller_notice', 'supervise', 'gate_seed',
  'linger_close', 'workspace_remove', 'merge_queue_publish', 'issue_branch', 'issue_suspend',
  'auto_merge_disable', 'ready_merge', 'check_wake', 'check_look', 'check_bound',
  'phase_ended', 'issue_bundle', 'issue_release'));

-- The look of the production check a claim's worker took: the look side of serving_generation and
-- serving_assignment, carried from the start row onto the delivery and, when a confirmed delivery
-- is retired, onto the claim. It is what says a completion is a check report rather than a phase
-- completion (api/handoff.go, supervise.Claim.ServingCheck), where before the route guessed from
-- the issue being done — which took the verdict of a phase completion that outlived the close for
-- a report on production. The check instance travels with the look because a later merge of the
-- same issue replaces the check and counts its looks from one again, so a look number alone names
-- no look (production_checks.opened_at, the check's id).
--
-- No backfill, as 0067's assignment columns had none: 0 is a claim, or a delivery, that is no
-- look's, which is what every row recorded before this migration was. A worker live at the
-- upgrade holding a look's task therefore serves no look: its report on that look is refused
-- CHECK_LOOK_NOT_SERVED, before the phase path, recording nothing, and the look's own bound
-- (check_look) ends it as one nobody reported; the next look's start row carries the look onto
-- the delivery.
alter table pending_task_deliveries add column check_look integer not null default 0;
alter table pending_task_deliveries add column check_opened_at timestamptz;
alter table claims add column serving_check_look integer not null default 0;
alter table claims add column serving_check_opened_at timestamptz;

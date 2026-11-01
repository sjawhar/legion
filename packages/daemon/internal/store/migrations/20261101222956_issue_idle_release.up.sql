-- 20261101222956_issue_idle_release.up.sql — the idle release of an issue pod (INFRA-945).
--
-- `issue_release` joins the outbox kinds: the act that saves the workspace's unpushed work as a
-- git bundle, then suspends the issue's Sandbox, waits its pod out and deletes the Sandbox with
-- the role Secrets it owns. It is its own kind, never `issue_suspend` reused and never the
-- `suspend` supervise op reused, because the open-issue invariant matches on kind and
-- payload->>'op' and a reader must be able to say which act it is reading: a close stops an issue
-- that has left the workflow, a release stops the pod of an issue that is still in it. The
-- constraint is rewritten whole, naming every kind, as 0049 and 0066 did.
alter table outbox drop constraint outbox_kind_check;
alter table outbox add constraint outbox_kind_check check (kind in (
  'dispatch_status', 'dispatch_message', 'notice', 'controller_notice', 'supervise', 'gate_seed',
  'linger_close', 'workspace_remove', 'merge_queue_publish', 'issue_branch', 'issue_suspend',
  'auto_merge_disable', 'ready_merge', 'check_compare', 'check_look', 'check_bound',
  'phase_ended', 'issue_release'));

-- issue_releases is the mark one release in progress leaves, written in the same transaction that
-- read the idle view and queued the release's rows, and deleted when the release finishes or is
-- cancelled. One row per issue: a second release of a pod already being released never starts.
--
-- boundary_row is the release's boundary, the outbox row that stops the first role process of the
-- pod: the release's own first row. An outbox row of the issue below it was written by a
-- transaction that was already in flight when the release was marked, so its delivery or start
-- was queued for a pod that still ran every role — it cancels the release, and the roles go on in
-- the same pod. A row above it was queued knowing the pod is going, so it waits behind the
-- release on the issue's lifecycle lane and resumes the pod once the release is complete. That is
-- the one place the boundary is read (store.IssueReleaseStop).
--
-- started_row is the newest last_start_row of the pod's claims when the release was marked. A
-- row below the boundary that the lane ran before the first role stop is gone from the outbox by
-- then, and what it left behind is a claim started above this mark: that start was queued before
-- the boundary too, so it cancels the release in the same place. Together the two are the whole
-- boundary rule (store.IssueReleaseStop).
create table issue_releases (
  project text not null,
  issue text not null,
  generation bigint not null check (generation >= 0),
  boundary_row bigint not null check (boundary_row > 0),
  started_row bigint not null check (started_row >= 0),
  role text not null check (role in ('architect', 'planner', 'implementer', 'tester', 'reviewer', 'merger')),
  marked_at timestamptz not null default now(),
  primary key (project, issue));

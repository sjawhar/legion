-- 20261102120000_pod_wakes.up.sql — what a wake of a released issue pod needs to act once and to
-- act at all: the wakes already taken, the backstop a release arms, and the Envoy listener's own
-- sign that it is alive and reporting.

-- One row per wake taken, so a wake that can be reported more than once wakes a pod once: a head
-- settles again as each of its checks finishes, and each settlement of that head is one key here.
-- The key is the wake's own identity within its kind — a head sha, a backstop row's id, the
-- listener's report id — and the kind keeps two sources from colliding on one key.
create table pod_wake_marks (
  project text not null,
  issue text not null,
  kind text not null,
  key text not null,
  at timestamptz not null,
  primary key (project, issue, kind, key)
);

-- One row per phase worker a release left with its phase open: the backstop that wakes it
-- backstop_minutes after the release whatever else happens. It is a stored row with a due time
-- rather than a timer, so a daemon restart keeps it and one that came due while the daemon was
-- down fires once the daemon is back. An architect gets none. fired is set by the pass that takes
-- it, under a conditional update, so one backstop fires once however many passes read it.
create table pod_wake_backstops (
  id bigserial primary key,
  project text not null,
  issue text not null,
  role text not null,
  release_id bigint not null default 0,
  due_at timestamptz not null,
  fired boolean not null default false
);
create index pod_wake_backstops_due on pod_wake_backstops (project, due_at) where not fired;
create unique index pod_wake_backstops_open on pod_wake_backstops (project, issue, role) where not fired;

-- The Envoy listener's positive liveness signal, one row per project, replaced by each
-- `listener_ready` report the daemon reads off notifications.legion.wake. The daemon releases
-- nothing while this is older than the freshness window: without it, "the listener sends no
-- reports" and "the listener has nothing to report" are the same silence, and a pod released into
-- that silence would never be woken by a role message or an agent message.
create table listener_reports (
  project text primary key,
  ready_at timestamptz not null
);

-- 20261102114500_pod_wakes.up.sql — the two durable facts a wake of a released issue pod rests
-- on: which wakes a daemon has already taken, and when the backstop of a release falls due.
--
-- pod_wake_marks is how a wake happens at most once. A head can settle several times as its
-- checks finish, and the Envoy listener reports an undeliverable message once per delivery
-- attempt, so the same wake reaches the daemon again and again; the first to insert its key takes
-- it and every later one finds the row. The key's meaning is the kind's: a settlement's is the
-- head it settled, a listener report's the id the listener gave it, the backstop's its release.
create table pod_wake_marks (
  project text not null,
  issue text not null,
  kind text not null,
  wake_key text not null,
  at timestamptz not null,
  primary key (project, issue, kind, wake_key)
);

-- pod_wake_backstops is the one row a finished release leaves so its pod comes back even when
-- nothing else ever wakes it: one per released issue, due backstop_minutes after the release. It
-- is a row rather than a timer in memory so a backstop that fell due while the daemon was down
-- fires once the daemon is up again. The row goes when the backstop fires, when another wake
-- brings the pod back, and when the release is cancelled.
create table pod_wake_backstops (
  project text not null,
  issue text not null,
  release_id bigint not null default 0,
  due_at timestamptz not null,
  primary key (project, issue)
);
create index pod_wake_backstops_due on pod_wake_backstops (project, due_at);

-- 20261102002500_pod_release_records.up.sql — what the daemon records of each issue pod it
-- released, each wake that brought one back, and each pod the idle rule kept past the idle
-- period. A deployment reads these to say whether release is working: how many pods were
-- released, how long a resume took from the wake to the woken role's first turn, and which pods
-- were held and why. An issue pod reads them through the daemon's API with the claim credential
-- it holds, so the production check can measure from inside the cluster.
create table pod_releases (
  id bigserial primary key,
  project text not null,
  issue text not null,
  generation bigint not null default 0,
  roles text not null default '',
  at timestamptz not null,
  bundle_bytes bigint not null default 0
);
create index pod_releases_project_at on pod_releases (project, at desc);

-- One row per wake of a released pod. release_id is the pod_releases row it ended, 0 when none is
-- recorded. pod_ready_at and first_turn_at are null until each is observed, and stuck is a
-- resume with no first turn ten minutes after its wake.
create table pod_resumes (
  id bigserial primary key,
  release_id bigint not null default 0,
  project text not null,
  issue text not null,
  role text not null,
  kind text not null,
  at timestamptz not null,
  pod_ready_at timestamptz,
  first_turn_at timestamptz,
  stuck boolean not null default false
);
create index pod_resumes_project_at on pod_resumes (project, at desc);

-- One row per streak of the idle rule keeping a pod for one reason, rather than one row per
-- pass: a pod held by an answer nobody has given or by an old image is held for days, and at one
-- tick a minute that would be 1,440 rows a day saying the same thing. The row is upserted on
-- (project, issue, reason), so first_at is when the rule first read that reason for that pod, at
-- is the newest pass, and passes counts them. reason stays the rule's own word
-- (workflow.IdleHold's String) so a reader can group by it, and detail carries what that one word
-- leaves out — the kinds a background-work hold's session reported — empty where there is none.
create table pod_holds (
  id bigserial primary key,
  project text not null,
  issue text not null,
  reason text not null,
  detail text not null default '',
  first_at timestamptz not null,
  at timestamptz not null,
  passes bigint not null default 1
);
create unique index pod_holds_project_issue_reason on pod_holds (project, issue, reason);
create index pod_holds_project_at on pod_holds (project, at desc);

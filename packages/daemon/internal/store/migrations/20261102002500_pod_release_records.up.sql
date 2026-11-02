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

-- One row per pass of the idle rule that kept a pod, with the rule's own name for the reason.
create table pod_holds (
  id bigserial primary key,
  project text not null,
  issue text not null,
  reason text not null,
  at timestamptz not null
);
create index pod_holds_project_at on pod_holds (project, at desc);

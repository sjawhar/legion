-- 20261103170923_legion_state.up.sql — what Dispatch records of each Legion daemon's state.
--
-- dispatch://INFRA-1077: Dispatch reads each Legion daemon's `GET /legion/v1/state` every minute
-- from one replica (internal/dispatch/legion, the leased `legion-state` loop), records what
-- changed, and shows a Legion view on the project Issues page with four metrics on the Delivery
-- page. This migration creates the three tables that reading holds: which daemon each project has,
-- how each read went, and every change a read found.
--
-- legion_settings is one row per project, the daemon Dispatch reads it from
-- (GET/PUT/DELETE /api/v1/settings/legion…, api/settings_legion.go).
--
-- - project is the project's own key and the row's identity: one daemon per project.
-- - state_url is the daemon's base URL; the sampler reads `state_url + /legion/v1/state` and the
--   operator's relay writes `state_url + /legion/v1/operator/settings`. The route holds it to an
--   absolute http(s) URL with a host and no path, as Dispatch holds DISPATCH_AGENT_SECRETS_URL.
-- - created_by is the actor that first set the project's setting, as repo_projects.created_by is
--   the actor that first mapped a repository; a later write of the same project changes the URL
--   and updated_at and keeps it.
-- - updated_at is when the row was last written.
--
-- legion_reads is one row per read, appended: the freshness the view shows and the last failure it
-- names. A read that failed keeps its bytes, so a daemon answering more than the sampler's
-- 8 MiB cap reads as a size, not as silence.
--
-- - ok and error are one fact written twice over: a read that failed names why and one that
--   succeeded names nothing, which the check holds.
-- - bytes is what the daemon sent, and for a read over the cap the point the read stopped (the cap
--   and one byte), since no reader counts past a body it refused.
-- - The index is the view's one read: the newest row of a project.
--
-- legion_observations is append-only, one row per value a read found changed: the sampler writes a
-- row only where the value differs from the newest row that project, issue and field already hold,
-- so a minute in which nothing moved writes nothing. The fields are `phase`, `status`, `slot`,
-- `waiting_on`, `claim.<role>`, `pull_request.state`, `pull_request.checks_verdict` and
-- `pull_request.requires_checks` (internal/dispatch/legion/state.go).
--
-- - issue_key is the daemon's own issue key and no foreign key: the daemon records issues of its
--   project, and Dispatch records what the daemon says whether or not it holds that issue itself.
-- - value is the field's value as JSON, `null` where the issue carries none, and the comparison
--   that decides whether to write a row is Postgres's own jsonb comparison, so no Go encoding
--   difference can write a row for a value that did not move.
-- - The unique index is the row's own identity, (project, issue_key, field, observed_at): one
--   value per field per moment, which also serves the newest-value lookup each write makes
--   (an ordered scan of its first three columns). The second index is the view's read of a
--   project's changes over a window.
--
-- Three new tables no earlier release names: nothing existing is read or rewritten, and the
-- release before this one never names them, so it rolls (cmd/dispatch/README.md, "Database
-- migrations") and the census answers 0. Each foreign key holds projects in SHARE ROW EXCLUSIVE
-- only while its table is created. The identifier is the UTC moment this was written, as
-- `migrationid` requires of every migration past the numbered series.
create table legion_settings (
  project text primary key references projects(key) on delete cascade,
  state_url text not null,
  created_by jsonb not null,
  updated_at timestamptz not null default now()
);

create table legion_reads (
  id bigserial primary key,
  project text not null references projects(key) on delete cascade,
  read_at timestamptz not null default now(),
  ok boolean not null,
  error text,
  bytes integer not null default 0 check (bytes >= 0),
  check (ok = (error is null))
);

create index legion_reads_project_read_at on legion_reads (project, read_at desc, id desc);

create table legion_observations (
  id bigserial primary key,
  project text not null references projects(key) on delete cascade,
  issue_key text not null,
  field text not null,
  value jsonb not null,
  observed_at timestamptz not null default now()
);

create unique index legion_observations_point on legion_observations (project, issue_key, field, observed_at);

create index legion_observations_project_observed_at on legion_observations (project, observed_at);

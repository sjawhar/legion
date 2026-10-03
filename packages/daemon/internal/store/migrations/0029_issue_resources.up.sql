-- 0029_issue_resources.up.sql — an issue owns one Agent Sandbox, its role Secrets and, for a
-- root issue, the tree PVC. Per-role release never deletes those resources: cleanup begins only
-- through the durable issue-resource lifecycle after the issue close fence has observed every
-- stored sibling claim.
create table issue_resources (
  project text not null,
  issue text not null,
  tree text not null,
  sandbox_name text not null,
  generation bigint not null check (generation >= 1),
  cleanup_started boolean not null default false,
  cleanup_generation bigint,
  cleanup_confirmed_at timestamptz,
  updated_at timestamptz not null default now(),
  primary key (project, issue)
);

-- A project changes layouts atomically. No process may create an issue pod against a database that
-- still holds a legacy per-claim Sandbox locator; startup writes this marker only after refusal
-- checks find no old layout.
create table runtime_layouts (
  project text primary key,
  layout text not null,
  updated_at timestamptz not null default now()
);

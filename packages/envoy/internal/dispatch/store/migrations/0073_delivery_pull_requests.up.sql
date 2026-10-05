-- 0073_delivery_pull_requests.up.sql
-- LEGION-567: one row per population pull request (LEGION-294's rule: authored by one of
-- delivery_settings.population_authors, merged, not in an excluded repository, not a task PR).
-- (repo, number) is the PR's identity, matching "owner/repo#N" everywhere else in Dispatch.
--
-- GitHub's pull_request webhook payload (envoy's internal/contracts normalize.go) carries no
-- created_at, merged_at, additions or deletions, so a row the live intake consumer writes
-- straight from a merged-PR event cannot have them yet: those four columns are nullable and
-- partial is true until the completing GitHub fetch (or, failing that, the next reconcile pass)
-- fills them in. The partial index lets that pass find incomplete rows without scanning the
-- table.
--
-- issue_key is a soft reference (on delete set null, matching issue_components' pattern in 0038):
-- a PR can name an issue that is later deleted, and the link should not take the PR row with it.
create table delivery_pull_requests (
  repo text not null,
  number integer not null,
  title text not null,
  url text not null,
  author text not null,
  created_at timestamptz,
  merged_at timestamptz,
  first_commit_at timestamptz,
  merge_commit_sha text,
  additions integer,
  deletions integer,
  rework boolean not null default false,
  issue_key text references issues(key) on delete set null,
  sessions text[] not null default '{}',
  partial boolean not null default false,
  updated_at timestamptz not null default now(),
  primary key (repo, number)
);

create index delivery_pull_requests_merged_at on delivery_pull_requests (merged_at) where merged_at is not null;
create index delivery_pull_requests_partial on delivery_pull_requests (repo, number) where partial;

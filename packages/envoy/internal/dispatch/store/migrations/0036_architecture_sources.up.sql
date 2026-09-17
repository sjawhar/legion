-- 0036_architecture_sources.up.sql
--
-- One architecture source per project: the repository/branch its architecture
-- documents are imported from (directory fixed at .dispatch/architecture/).
-- installation_id caches the GitHub App installation the access check
-- resolved; the importer re-resolves it on sync. last_sync_at, last_commit,
-- and last_error stay null until the importer fills them.
create table architecture_sources (
  project_key text primary key references projects(key) on delete cascade,
  repo text not null,
  branch text not null,
  enabled boolean not null default true,
  installation_id bigint not null,
  created_by jsonb not null,
  created_at timestamptz not null default now(),
  last_sync_at timestamptz,
  last_commit text,
  last_error text
);

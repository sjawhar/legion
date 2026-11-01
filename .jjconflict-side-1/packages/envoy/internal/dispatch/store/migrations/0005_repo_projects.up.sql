-- 0005_repo_projects.up.sql
create table repo_projects (
  repo text primary key,
  project text not null references projects(key),
  created_by jsonb not null,
  created_at timestamptz not null default now()
);

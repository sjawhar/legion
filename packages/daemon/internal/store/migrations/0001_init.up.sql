-- 0001_init.up.sql — the daemon's own state: which migrations it has applied,
-- and every time it booted. Later stages add tables; nothing here is reversed.
create table schema_version (
  version integer primary key,
  applied_at timestamptz not null default now());
create table daemon_boot (
  id bigserial primary key,
  project text not null,
  started_at timestamptz not null,
  stopped_at timestamptz);

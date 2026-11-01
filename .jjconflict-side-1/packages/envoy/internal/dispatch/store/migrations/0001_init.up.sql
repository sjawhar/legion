-- 0001_init.up.sql
create table users (
  login text primary key, access_token text not null, refresh_token text,
  access_expires_at timestamptz, refresh_expires_at timestamptz,
  created_at timestamptz not null default now(), updated_at timestamptz not null default now());
create table projects (
  key text primary key check (key ~ '^[A-Z][A-Z0-9]{1,9}$'), name text not null,
  next_number integer not null default 1, created_at timestamptz not null default now());
create table issues (
  key text primary key, project_key text not null references projects(key), number integer not null,
  title text not null, status text not null default 'triage', labels text[] not null default '{}',
  parent_key text references issues(key), route text, created_by jsonb not null,
  created_at timestamptz not null default now(), updated_at timestamptz not null default now(),
  closed_at timestamptz, last_seq integer not null default 0, unique (project_key, number));
create table issue_external_links (
  issue_key text not null references issues(key) on delete cascade, url text not null, kind text not null,
  primary key (issue_key, url));
create unique index issue_external_links_url on issue_external_links (url);
create table artifacts (
  id uuid primary key default gen_random_uuid(), issue_key text not null references issues(key),
  slug text not null, name text not null, kind text not null check (kind in ('doc','image','file')),
  is_primary boolean not null default false, created_by jsonb not null,
  created_at timestamptz not null default now(), unique (issue_key, slug));
create unique index artifacts_one_primary on artifacts (issue_key) where is_primary;
alter table artifacts add constraint artifacts_primary_is_doc check (not is_primary or kind = 'doc');
create table artifact_versions (
  id uuid primary key default gen_random_uuid(), artifact_id uuid not null references artifacts(id),
  number integer not null, markdown text, content bytea, mime text, size integer, sha256 text,
  authors jsonb not null default '[]', named boolean not null default false, summary text,
  created_at timestamptz not null default now(), unique (artifact_id, number));
create table doc_updates (            -- ygo VersionedPersistence
  artifact_id uuid not null references artifacts(id), version bigint not null, update bytea not null,
  created_at timestamptz not null default now(), primary key (artifact_id, version));
create table doc_snapshots (
  artifact_id uuid not null references artifacts(id), name text not null, version bigint not null,
  state bytea not null, created_at timestamptz not null default now(), primary key (artifact_id, name));
create table doc_checkpoints (artifact_id uuid primary key references artifacts(id), ceiling bigint not null, state bytea);
create table asks (
  id uuid primary key default gen_random_uuid(), issue_key text not null references issues(key),
  author jsonb not null, question text not null, options jsonb not null default '[]',
  multiple boolean not null default false, custom boolean not null default true,
  urgency text not null default 'med', anchor jsonb, state text not null default 'open',
  answer jsonb, created_at timestamptz not null default now());
create index asks_open on asks (issue_key) where state = 'open';
create table comments (
  id uuid primary key default gen_random_uuid(), issue_key text not null references issues(key),
  author jsonb not null, body text not null, anchor jsonb, reply_to uuid references comments(id),
  resolved boolean not null default false, suggestion jsonb, created_at timestamptz not null default now());
create table messages (
  id uuid primary key default gen_random_uuid(), issue_key text not null references issues(key),
  author jsonb not null, body text not null, created_at timestamptz not null default now());
create table refs (
  from_kind text not null, from_id text not null, to_kind text not null, to_id text not null,
  primary key (from_kind, from_id, to_kind, to_id));
create index refs_to on refs (to_kind, to_id);
create table events (
  id bigserial primary key, issue_key text not null references issues(key), seq integer not null,
  type text not null, actor jsonb not null, payload jsonb not null, notify boolean not null,
  published_at timestamptz, created_at timestamptz not null default now(), unique (issue_key, seq));
create index events_unpublished on events (id) where notify and published_at is null;
create table user_issue_state (
  login text not null, issue_key text not null references issues(key), pinned boolean not null default false,
  last_read_seq integer not null default 0, dismissed jsonb not null default '[]',
  primary key (login, issue_key));

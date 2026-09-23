-- 0003_record.up.sql — the durable issue workflow record and transactional outbox.
--
-- These are the Go daemon's own facts. Dispatch and GitHub remain authoritative for their
-- resources; this schema records the observations and effects that make one workflow durable.
create table issues (
  key text primary key,
  tree text not null,
  project text not null,
  title text not null,
  parent text,
  phase text not null check (phase in (
    'admitted', 'planning', 'implementing', 'testing', 'reviewing', 'retro', 'merging',
    'awaiting_merge', 'production_check', 'done', 'held')),
  generation bigint not null check (generation >= 0),
  status text not null,
  rank text not null,
  linger_until timestamptz,
  held_from text check (held_from in (
    'admitted', 'planning', 'implementing', 'testing', 'reviewing', 'retro', 'merging',
    'awaiting_merge', 'production_check', 'done')),
  last_dispatch_seq bigint not null,
  ready_pending_version integer check (ready_pending_version > 0));

create table phases (
  issue text not null references issues (key) on delete cascade,
  role text not null check (role in ('architect', 'planner', 'implementer', 'tester', 'reviewer', 'merger')),
  claim text not null,
  handoff_commit text not null,
  rounds integer not null check (rounds >= 0),
  verdict text not null,
  primary key (issue, role));

create table pull_requests (
  issue text primary key references issues (key) on delete cascade,
  repo text not null,
  number integer not null check (number > 0),
  branch text not null,
  head_sha text not null,
  head_updated_at timestamptz not null,
  head_updated_at_source text not null,
  verdict text not null,
  failing jsonb not null,
  failing_statuses jsonb not null,
  review_decision text not null,
  fix_attempts integer not null check (fix_attempts >= 0),
  blocked_attempts integer not null check (blocked_attempts >= 0),
  check_runs jsonb not null,
  generation bigint not null check (generation >= 0),
  snapshot text not null,
  reconciled boolean not null,
  pending_push jsonb,
  head_counted text not null);
create unique index pull_requests_repo_branch on pull_requests (repo, branch);

create table design_gates (
  issue text primary key references issues (key) on delete cascade,
  artifact_id text not null,
  latest_version integer not null check (latest_version > 0),
  approved_version integer check (approved_version > 0));

create table slots (
  issue text primary key references issues (key) on delete cascade,
  index integer not null check (index >= 0) unique,
  admitted_at timestamptz not null);

create table processed_events (
  source text not null,
  event_id text not null,
  processed_at timestamptz not null default now(),
  primary key (source, event_id));

create table outbox (
  id bigserial primary key,
  kind text not null check (kind in ('dispatch_status', 'dispatch_message', 'notice', 'supervise', 'gate_seed', 'linger_close', 'workspace_remove')),
  issue text not null,
  payload jsonb not null,
  attempts integer not null check (attempts >= 0),
  next_at timestamptz not null,
  last_error text not null,
  created_at timestamptz not null default now(),
  lease_token text not null default '',
  lease_until timestamptz);
create index outbox_due on outbox (next_at, id);

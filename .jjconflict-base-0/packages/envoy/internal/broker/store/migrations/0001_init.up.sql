-- packages/envoy/internal/broker/store/migrations/0001_init.up.sql
create table if not exists broker_schema_migrations (
  version integer primary key,
  applied_at timestamptz not null default now()
);

create table if not exists launcher_credentials (
  id uuid primary key,
  operator text,                       -- github login; null for a service credential
  service text,                        -- e.g. legion-daemon; a service credential enrolls pods only
  host text not null,
  token_hash bytea not null unique,    -- sha256 of the bearer token
  issued_via_ask text,                 -- dispatch ask id that approved issuance
  created_at timestamptz not null default now(),
  revoked_at timestamptz
);

create table if not exists launcher_credential_requests (
  pending_id_hash bytea primary key,   -- sha256 of the capability handed to the CLI
  operator text not null,
  host text not null,
  ask_id text not null,
  ask_edited_at text,
  state text not null check (state in ('pending','issued','denied','expired')),
  credential_id uuid references launcher_credentials(id),
  token_once text,                     -- cleared on first read
  created_at timestamptz not null default now(),
  expires_at timestamptz not null
);

create table if not exists enrollments (
  id uuid primary key,
  kind text not null check (kind in ('box','host','pod')),
  runtime_id text not null,
  operator text,
  approver_kind text not null check (approver_kind in ('operator','issue_assignee')),
  approver_issue text,
  thumbprint text not null,            -- base64url sha256 JWK thumbprint
  session_id text,
  launcher_credential_id uuid not null references launcher_credentials(id),
  created_at timestamptz not null default now(),
  lease_expires_at timestamptz not null,
  revoked_at timestamptz
);
create unique index if not exists enrollments_live_runtime on enrollments (launcher_credential_id, runtime_id) where revoked_at is null;

create table if not exists requests (
  id uuid primary key,
  enrollment_id uuid not null references enrollments(id),
  issue_key text not null,
  reason text not null,
  state text not null check (state in ('pending','granted','denied','cancelled','expired')),
  allowed_approver text,               -- login who may approve; null when nothing needs approval
  ask_id text,
  ask_edited_at text,
  rules_version text not null,
  lifetime_seconds integer not null,
  session_id text,                     -- optional Envoy session id from the request body, for the wake only
  created_at timestamptz not null default now(),
  pending_expires_at timestamptz,
  decided_at timestamptz,
  decided_by text,
  decision_detail text
);
create index if not exists requests_pending on requests (state) where state = 'pending';

create table if not exists request_secrets (
  request_id uuid not null references requests(id),
  name text not null,
  decision text not null check (decision in ('automatic','approval','deny')),
  delivery text not null check (delivery in ('inject','proxy')),
  source text not null,
  primary key (request_id, name)
);

create table if not exists grants (
  id uuid primary key,
  request_id uuid not null references requests(id),
  enrollment_id uuid not null references enrollments(id),
  approver text,
  created_at timestamptz not null default now(),
  expires_at timestamptz not null,
  revoked_at timestamptz,
  revoked_by text
);

create table if not exists proof_jtis (
  jti text primary key,
  expires_at timestamptz not null
);
create index if not exists proof_jtis_expiry on proof_jtis (expires_at);

create table if not exists audit (
  id bigserial primary key,
  at timestamptz not null default now(),
  kind text not null,
  enrollment_id uuid,
  request_id uuid,
  grant_id uuid,
  actor text not null,
  detail jsonb not null default '{}'::jsonb
);
create index if not exists audit_request on audit (request_id);

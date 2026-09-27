-- packages/envoy/internal/broker/store/migrations/0005_credential_requests.up.sql
-- AGENTC-393 v9: credential-request records replace Dispatch asks.
create table if not exists credential_requests (
  id text primary key,                -- lowercase-hex sha256 of body
  body text not null,                 -- canonical body, verbatim
  kind text not null check (kind in ('agent_secret','launcher_credential')),
  approver text not null,
  enrollment_id uuid references enrollments(id),
  code text,
  created_at timestamptz not null default now(),
  expires_at timestamptz not null
);
create or replace function credential_requests_refuse_update() returns trigger language plpgsql as
$$ begin raise exception 'credential requests are append-only'; end $$;
create trigger credential_requests_no_update before update or delete on credential_requests
  for each row execute function credential_requests_refuse_update();

create table if not exists credential_request_events (
  id bigserial primary key,
  record_id text not null references credential_requests(id),
  event text not null check (event in ('approved','denied','expired','cancelled','revoked')),
  assertion jsonb,
  credential_id text,
  actor text not null,
  detail text,
  at timestamptz not null default now()
);
create unique index if not exists credential_request_decision on credential_request_events (record_id)
  where event in ('approved','denied','expired','cancelled');

create table if not exists approver_keys (
  credential_id text primary key,
  login text not null,
  aaguid uuid not null,
  cose_key bytea not null,
  registration jsonb not null,
  challenge_nonce text not null,
  endorsed_by text,
  seeded boolean not null default false,
  sign_count bigint not null default 0,
  state text not null default 'active' check (state in ('active','tombstoned','revoked')),
  registered_at timestamptz not null default now(),
  last_used_at timestamptz,
  removed_at timestamptz
);
create index if not exists approver_keys_login on approver_keys (login) where state = 'active';

create table if not exists approver_key_seeds (
  login text not null,
  credential_id text not null,
  created_at timestamptz not null default now(),
  primary key (login, credential_id)
);

create table if not exists webauthn_ceremonies (
  id uuid primary key,
  login text not null,
  kind text not null check (kind in ('register','endorse')),
  nonce text,
  challenge bytea not null,
  subject text,
  created_at timestamptz not null default now(),
  expires_at timestamptz not null
);

alter table launcher_credentials drop column token_hash;
alter table launcher_credentials add column key_thumbprint text not null;
alter table launcher_credentials add column public_jwk jsonb not null;
alter table launcher_credentials add column record_id text references credential_requests(id);
alter table launcher_credentials add column expires_at timestamptz not null;
create unique index if not exists launcher_credentials_live_key on launcher_credentials (key_thumbprint)
  where revoked_at is null;

drop table launcher_credential_requests;
create table if not exists machine_login_polls (
  pending_id_hash bytea primary key,
  record_id text not null references credential_requests(id),
  created_at timestamptz not null default now()
);

alter table enrollments drop column approver_kind;
alter table enrollments drop column approver_issue;

alter table requests add column record_id text references credential_requests(id);
alter table requests drop column ask_id;
alter table requests drop column ask_edited_at;
alter table requests drop column issue_key;

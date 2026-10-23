-- packages/envoy/internal/broker/store/migrations/0016_oauth_machine_login.up.sql
-- The machine login's OAuth 2.0 state (authelia.com/provider/oauth2): device codes, access and
-- refresh token sessions, DPoP proof replay and nonces, and the provider's HMAC secret.
-- A session row names its launcher credential (credential_id) once the login is approved, so a
-- revoke ends the credential's refresh tokens in the same transaction that revokes it.
create table oauth_device_codes (
  signature text primary key,
  user_code_signature text not null unique,
  request jsonb not null,
  status smallint not null default 0,
  expires_at timestamptz not null
);
create index oauth_device_codes_expires on oauth_device_codes (expires_at);
create table oauth_access_tokens (
  signature text primary key,
  request_id text not null,
  credential_id uuid references launcher_credentials(id),
  request jsonb not null,
  active boolean not null default true,
  expires_at timestamptz not null
);
create index oauth_access_tokens_request on oauth_access_tokens (request_id);
create index oauth_access_tokens_expires on oauth_access_tokens (expires_at);
create table oauth_refresh_tokens (
  signature text primary key,
  request_id text not null,
  access_signature text,
  credential_id uuid references launcher_credentials(id),
  request jsonb not null,
  active boolean not null default true,
  -- A rotated token stays redeemable until this moment, so a response lost in transit can be
  -- retried once instead of revoking the whole login (RotateRefreshToken's grace period).
  grace_until timestamptz,
  expires_at timestamptz not null
);
create index oauth_refresh_tokens_request on oauth_refresh_tokens (request_id);
create index oauth_refresh_tokens_credential on oauth_refresh_tokens (credential_id) where active;
-- The sweep walks the table in this order with a cursor, a batch at a time.
create index oauth_refresh_tokens_sweep on oauth_refresh_tokens (expires_at, signature);
create table oauth_dpop_proofs (
  jti text not null,
  htu text not null,
  expires_at timestamptz not null,
  primary key (jti, htu)
);
create index oauth_dpop_proofs_expires on oauth_dpop_proofs (expires_at);
create table oauth_dpop_nonces (
  nonce text primary key,
  expires_at timestamptz not null
);
create index oauth_dpop_nonces_expires on oauth_dpop_nonces (expires_at);
create table oauth_provider_secret (
  id boolean primary key default true check (id),
  secret bytea not null check (octet_length(secret) >= 32)
);

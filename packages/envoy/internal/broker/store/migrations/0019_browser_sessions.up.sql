-- packages/envoy/internal/broker/store/migrations/0019_browser_sessions.up.sql
-- A person's session on the broker's own approval page, made when they sign in to the broker.
-- Their browser holds a random secret in a cookie; the row keeps only that secret's SHA-256, so a
-- read of this table hands no one a cookie that signs in. A session lasts until expires_at, an
-- absolute expiry set at sign-in and never extended, or until its logout deletes the row; a row
-- past its expiry signs no one in, and the broker's sweeper deletes it through the expires index.
create table browser_sessions (
  secret_hash bytea primary key check (octet_length(secret_hash) = 32),
  login text not null check (login <> ''),
  created_at timestamptz not null default now(),
  expires_at timestamptz not null
);
create index browser_sessions_expires on browser_sessions (expires_at);

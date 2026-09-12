-- 0017_agent_tokens.up.sql
create table agent_tokens (
  id uuid primary key default gen_random_uuid(),
  owner text not null,
  name text not null,
  token_hash bytea not null unique,
  prefix text not null,
  created_at timestamptz not null default now(),
  last_used_at timestamptz,
  revoked_at timestamptz
);
create index agent_tokens_owner_created_at on agent_tokens (owner, created_at desc);

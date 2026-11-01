-- 0002_claims.up.sql — role claims and the one task each may have pending.
--
-- A claim row is everything supervision needs to pick an agent back up after a restart: where
-- its process is (the locator, validated when read back), which launch it is on (generation, and
-- the hash of that launch's boot token the shim's hello is resolved by), which agent session it
-- must resume, the secret its registration was issued (as a hash), and its budgets.
create table claims (
  token text primary key,
  project text not null,
  tree text not null,
  issue text not null,
  role text not null check (role in ('architect', 'planner', 'implementer', 'tester', 'reviewer', 'merger')),
  generation bigint not null check (generation >= 0),
  session text not null,
  session_file text not null,
  locator jsonb,
  state text not null check (state in (
    'queued', 'launching', 'shim_connected', 'registered', 'ready',
    'working', 'idle', 'suspended', 'failed', 'retired')),
  launch_failures integer not null check (launch_failures >= 0),
  prompt_failures integer not null check (prompt_failures >= 0),
  prompt_retires integer not null check (prompt_retires >= 0),
  boot_token_hash bytea,
  capability_hash bytea,
  uncertain_streak integer not null check (uncertain_streak >= 0),
  updated_at timestamptz not null default now());
create unique index claims_boot_token_hash on claims (boot_token_hash) where boot_token_hash is not null;

-- One row per claim: the task queued for it. delivered_at is the acknowledgement of the latest
-- send; confirmed_at is the turn that send started, and it stays until that turn ends, because a
-- refusal that arrives after the acknowledgement takes the confirmation back.
create table pending_task_deliveries (
  claim_token text primary key references claims (token) on delete cascade,
  delivery_id text not null,
  task text not null,
  queued_at timestamptz not null,
  delivered_at timestamptz,
  confirmed_at timestamptz);

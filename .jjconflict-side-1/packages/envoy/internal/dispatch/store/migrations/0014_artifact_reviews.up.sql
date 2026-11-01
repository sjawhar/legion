alter table asks add column kind text not null default 'question' check (kind in ('question', 'approval'));
alter table asks add column approval jsonb;
create table artifact_reviews (
  id uuid primary key default gen_random_uuid(),
  artifact_id uuid not null references artifacts(id),
  version integer not null,
  state text not null check (state in ('approved', 'changes_requested')),
  actor jsonb not null,
  reason text,
  ask_id uuid references asks(id),
  created_at timestamptz not null default now()
);
create index artifact_reviews_by_artifact on artifact_reviews (artifact_id, created_at desc, id desc);
create index asks_open_approvals on asks ((approval->>'artifact_id')) where kind = 'approval' and state = 'open';

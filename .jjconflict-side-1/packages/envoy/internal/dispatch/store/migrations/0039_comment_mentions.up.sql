create table comment_mentions (
  comment_id uuid not null references comments(id),
  target text not null,
  delivery text not null check (delivery in ('btw', 'aside', 'steer')),
  resolved_session_id text,
  primary key (comment_id, target)
);

create table comment_deliveries (
  comment_id uuid not null references comments(id),
  target text not null,
  attempt int not null,
  delivery text not null check (delivery in ('btw', 'aside', 'steer')),
  session_id text,
  envelope_id text,
  state text not null check (state in ('pending', 'sent', 'failed')),
  error text,
  resolve_error text,
  reply_id uuid references comments(id),
  created_at timestamptz not null default now(),
  primary key (comment_id, target, attempt)
);

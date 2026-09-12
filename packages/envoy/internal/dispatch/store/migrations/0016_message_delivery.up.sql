alter table messages rename column reply_to to in_reply_to;
alter table messages rename constraint messages_reply_to_fkey to messages_in_reply_to_fkey;
alter index messages_reply_to rename to messages_in_reply_to;
alter table messages add column target text;

create table message_deliveries (
  message_id uuid not null references messages(id),
  attempt int not null,
  delivery text not null check (delivery in ('btw', 'aside', 'steer')),
  session_id text not null,
  envelope_id text,
  state text not null check (state in ('sent', 'failed')),
  error text,
  reply_id uuid references messages(id),
  created_at timestamptz not null default now(),
  primary key (message_id, attempt)
);

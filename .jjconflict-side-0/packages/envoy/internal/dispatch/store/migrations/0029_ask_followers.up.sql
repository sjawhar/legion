-- 0029_ask_followers.up.sql
-- Every session that writes to an ask follows it: the outbox delivers the ask's answer,
-- edits, resolution, and replies to each follower's own topic. Backfilled from the asks
-- sessions opened and the ask replies sessions wrote.
create table ask_followers (
  ask_id uuid not null references asks(id) on delete cascade,
  session_id text not null,
  since timestamptz not null default now(),
  primary key (ask_id, session_id)
);

insert into ask_followers (ask_id, session_id, since)
select id, author->>'id', created_at
from asks
where author->>'kind' = 'session' and coalesce(author->>'id', '') <> ''
on conflict do nothing;

insert into ask_followers (ask_id, session_id, since)
select ask_id, author->>'id', min(created_at)
from comments
where ask_id is not null and author->>'kind' = 'session' and coalesce(author->>'id', '') <> ''
group by ask_id, author->>'id'
on conflict do nothing;

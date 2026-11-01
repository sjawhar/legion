-- 0020_outbox_retry.up.sql
alter table events add column attempt_count integer not null default 0;
alter table events add column next_attempt_at timestamptz;
alter table events add column published_destinations text[] not null default '{}';
create index events_unpublished_ready on events (next_attempt_at, id) where published_at is null;

create table user_sessions (
  login text primary key,
  generation bigint not null default 0
);

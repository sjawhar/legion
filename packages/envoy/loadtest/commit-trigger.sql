-- Commit-time trigger for loadgen's delivery-from-commit latency (loadtest/delivery.go).
--
-- events rows are updated by the outbox after insert (published_at), so an events row's xmin is
-- not the transaction that inserted it. This trigger copies each new event that carries a load
-- marker (lgm<N>x, the first one in its payload) into loadtest.event_commits inside the inserting
-- transaction, so pg_xact_commit_timestamp(xmin) of that row is the event's commit time
-- (track_commit_timestamp = on). It runs inside the transaction, and the commit-order lock, that
-- it measures: one small insert, no round trip. Events without a marker (the seed's, Dispatch's
-- own) insert nothing.
--
-- events: id bigserial, type text, payload jsonb
-- (internal/dispatch/store/migrations/0001_init.up.sql).
-- stack.sh installs it after the seed's outbox has drained: loadgen trigger-sql | psql ...

create schema if not exists loadtest;

create table if not exists loadtest.event_commits (
  event_id bigint primary key,
  type text not null,
  marker text not null
);

create or replace function loadtest.record_event_commit() returns trigger
language plpgsql as $$
declare
  found_marker text;
begin
  found_marker := substring(new.payload::text from 'lgm[0-9]+x');
  if found_marker is not null then
    insert into loadtest.event_commits (event_id, type, marker) values (new.id, new.type, found_marker);
  end if;
  return null;
end
$$;

drop trigger if exists loadtest_event_commits on public.events;

create trigger loadtest_event_commits
  after insert on public.events
  for each row execute function loadtest.record_event_commit();

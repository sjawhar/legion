-- Commit-time trigger for loadgen's delivery-from-commit latency (loadtest/delivery.go).
--
-- events rows are updated by the outbox after insert (published_at), so an events row's xmin is
-- not the transaction that inserted it. This trigger copies each new event that carries a load
-- marker (the first lgm[0-9]+x in its payload) into public.loadtest_event_commits inside the
-- inserting transaction, so pg_xact_commit_timestamp(xmin) of that row is the event's commit time
-- (track_commit_timestamp = on). It runs inside the transaction, and the commit-order lock, that
-- it measures: one small insert, no round trip. Events without a marker (the seed's, Dispatch's
-- own) insert nothing.
--
-- A marker is lgm<nonce><counter>x (loadtest/marker.go): the run's 9-digit nonce, then the write's
-- counter in the run. The pattern extracts the whole marker, nonce included, so each run reads
-- back only its own rows, also from a database that earlier runs wrote to without a reset.
--
-- events: id bigserial, type text, payload jsonb
-- (internal/dispatch/store/migrations/0001_init.up.sql).
-- loadgen seed-finish installs it after the seed's outbox has drained; loadgen trigger-sql prints it.
--
-- Every object below is in public, the schema Dispatch's own tables are in, and this file creates
-- no schema. A schema named as the database's login is - the deployment's login is loadtest -
-- stands in front of public in the default search_path ("$user", public), so creating one would
-- shadow public for every unqualified statement the server itself runs afterwards, Dispatch's own
-- create table if not exists among them (dispatch://INFRA-988).

create table if not exists public.loadtest_event_commits (
  event_id bigint primary key,
  type text not null,
  marker text not null
);

create or replace function public.loadtest_record_event_commit() returns trigger
language plpgsql as $$
declare
  found_marker text;
begin
  found_marker := substring(new.payload::text from 'lgm[0-9]+x');
  if found_marker is not null then
    insert into public.loadtest_event_commits (event_id, type, marker) values (new.id, new.type, found_marker);
  end if;
  return null;
end
$$;

drop trigger if exists loadtest_event_commits on public.events;

create trigger loadtest_event_commits
  after insert on public.events
  for each row execute function public.loadtest_record_event_commit();

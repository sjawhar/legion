-- A person's direct message becomes a session's own user turn only once Dispatch records that the
-- session took it (LEGION-394): the session accepts one attempt with POST
-- /api/v1/messages/{id}/deliveries/{attempt}/accept and injects the message only when that
-- succeeds.

-- requested_by is the actor whose send opened the attempt: the person who wrote or retried the
-- message, or the session a bearer retry named. A resume keeps it. A row written before this
-- column records nobody, and the accept reads that as not a person; it is never backfilled from
-- the message's author, since a bearer could have asked for that attempt.
alter table message_deliveries add column requested_by jsonb;

-- accepted_at and accepted_as record the one attempt of a message a session took, and what it
-- took it as. They are set together or not at all.
alter table message_deliveries add column accepted_at timestamptz;
alter table message_deliveries add column accepted_as text;
alter table message_deliveries add constraint message_deliveries_accepted_as_check
  check (accepted_as in ('user_turn'));
alter table message_deliveries add constraint message_deliveries_accepted_check
  check ((accepted_at is null) = (accepted_as is null));

-- A message is taken at most once, whichever of its attempts carried it.
create unique index message_deliveries_one_acceptance on message_deliveries (message_id)
  where accepted_at is not null;

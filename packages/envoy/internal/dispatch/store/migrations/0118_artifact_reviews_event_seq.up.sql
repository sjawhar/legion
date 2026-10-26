-- 0118_artifact_reviews_event_seq.up.sql
-- The sequence of the artifact.approved or artifact.changes_requested event each review appended on
-- its document's owner (its issue, else the document). The owner's row lock orders those events as
-- they commit, so the sequence places a review among its issue's other events where created_at, the
-- transaction's start, cannot: a review whose transaction began before another event's and committed
-- after it has the later sequence and the earlier time. The approval read returns it as `seq`, by
-- which a reader orders a review against the issue's other events. Null for a review recorded
-- before this column, and nullable, so the release before it keeps inserting reviews without it. The
-- column and its check are two statements: an inline check is validated at once, which reads every
-- row of the table under its ACCESS EXCLUSIVE lock, while `not valid` writes the catalog alone and
-- still refuses every row written or updated after it, which is the rule for a constraint a
-- migration adds to a table that already has rows (packages/envoy/cmd/dispatch/README.md). No
-- release validates it: every row below it carries no sequence at all.
alter table artifact_reviews add column event_seq bigint;
alter table artifact_reviews add constraint artifact_reviews_event_seq_positive check (event_seq > 0) not valid;

-- The index in docs.ReviewOrder's own order, per document, so a read of a document's newest review
-- (docs.LatestApprovedReview, latestReviewBy) takes the first entry of the document's range rather
-- than reading and sorting every review of it. It is a plain `create index`: the runner applies
-- each migration inside one transaction, where `concurrently` cannot run, and this transaction
-- already holds the table's ACCESS EXCLUSIVE lock from the statements above, so the build adds no
-- lock and holds that one for as long as it reads the table's rows, which the census holds to its
-- size limit. artifact_reviews_by_artifact (migration 0014) stays: the release before this one
-- orders by created_at alone while it still serves.
create index artifact_reviews_by_artifact_event_seq on artifact_reviews (artifact_id, event_seq desc nulls last, created_at desc, id desc);

-- Every review written once this migration commits carries its event's sequence, whichever release
-- wrote it. Dispatch's app rolls above one replica, so during a rollout, and for as long as a
-- rollback lasts, pods of the release before this one write reviews too, naming no event_seq. Left
-- null, such a review would rank below every sequenced one in docs.ReviewOrder (`event_seq desc
-- nulls last`) though it is newer: a person's approval written by an old pod would read back as the
-- changes requested before it, and a changes request written by an old pod would be overridden by
-- the approval before it. This trigger gives every such row its sequence in its own transaction, so
-- every sequenced row is newer than every unsequenced one: a row without a sequence was committed
-- before this migration, whose ACCESS EXCLUSIVE lock on artifact_reviews waits for every
-- transaction that has inserted a review and holds back every one that has not.
--
-- It is a constraint trigger deferred to the commit, because every release inserts a review before
-- it appends the review's event (docs.WriteReview), so the event exists only by then. At the commit
-- the transaction still holds its event owner's row lock, taken by its first append to that owner
-- and kept to the commit, so the transaction's own events on that owner are the newest ones: the
-- function reads the owner's events newest first while they are this transaction's own and takes
-- the oldest one of the review's type, document and version that no review of the document holds
-- yet, so two reviews written in one transaction take their events in the order they appended them.
--
-- An event is this transaction's own when it carries this transaction's now() (the
-- events.created_at default, which no writer sets) and its row was written by this transaction or
-- one of its subtransactions, which artifact_reviews_own_row reads from the row's xmin. now() alone
-- is the transaction's start, which another transaction that started in the same microsecond
-- shares: its committed event of the same type, document and version, one no review holds (a
-- review committed before this migration, which no trigger sequenced), would otherwise be taken as
-- the oldest match. xmin alone is not enough either, read as pg_current_xact_id(): a write in a
-- savepoint stamps the subtransaction's xid, which is not the transaction's own. So the row's xmin
-- is read through pg_xact_status, which answers `in progress` for this transaction and each of its
-- subtransactions and `committed` for every other transaction whose row this snapshot can see,
-- since a row of a transaction still in progress elsewhere is not visible to it. A row a savepoint
-- rolled back is not visible either, so its event is no one's. The time check comes first, so the
-- xid is read only for a row written within the same microsecond, never for an old or frozen one.
-- A row this release's docs.WriteReview has already given its sequence is left as it is, and a row
-- whose transaction appended no such event (a test's bare insert) stays null.
create function artifact_reviews_own_row(row_xmin xid) returns boolean language plpgsql as $$
declare
  -- The xid8 of row_xmin: its epoch is the current transaction's, or the one either side of it
  -- when the two xids sit across a wraparound; a row of this transaction is within 2^31 of it.
  here bigint := pg_current_xact_id()::text::bigint;
  widened bigint := ((here >> 32) << 32) + row_xmin::text::bigint;
begin
  if widened > here + 2147483648 then
    widened := widened - 4294967296;
  elsif widened < here - 2147483648 then
    widened := widened + 4294967296;
  end if;
  if widened < 0 then
    return false;
  end if;
  return pg_xact_status(widened::text::xid8) is not distinct from 'in progress';
end $$;

-- The trigger is on artifact_reviews, the one table this migration already locks, so the migration
-- holds no second table's lock: a trigger on events would also wait for SHARE ROW EXCLUSIVE on
-- events while holding artifact_reviews, behind a transaction that appended an event and then waits
-- to insert its review (a carried approval after its ask moved), which waits on this migration.
create function artifact_reviews_event_seq() returns trigger language plpgsql as $$
declare
  wanted text := case new.state when 'approved' then 'artifact.approved' else 'artifact.changes_requested' end;
  owner_issue text;
  candidate record;
  found_seq bigint;
begin
  perform 1 from artifact_reviews where id = new.id and event_seq is null;
  if not found then
    return null;
  end if;
  select issue_key into owner_issue from artifacts where id = new.artifact_id;
  if owner_issue is not null then
    for candidate in
      select e.seq, e.type, e.payload, e.created_at, e.xmin from events e where e.issue_key = owner_issue order by e.seq desc
    loop
      exit when candidate.created_at <> now();
      exit when not artifact_reviews_own_row(candidate.xmin);
      if candidate.type = wanted
        and candidate.payload->>'artifact_id' = new.artifact_id::text
        and candidate.payload->'version' = to_jsonb(new.version)
        and not exists (select 1 from artifact_reviews r where r.artifact_id = new.artifact_id and r.event_seq = candidate.seq)
      then
        found_seq := candidate.seq;
      end if;
    end loop;
  else
    for candidate in
      select e.seq, e.type, e.payload, e.created_at, e.xmin from events e where e.artifact_id = new.artifact_id order by e.seq desc
    loop
      exit when candidate.created_at <> now();
      exit when not artifact_reviews_own_row(candidate.xmin);
      if candidate.type = wanted
        and candidate.payload->>'artifact_id' = new.artifact_id::text
        and candidate.payload->'version' = to_jsonb(new.version)
        and not exists (select 1 from artifact_reviews r where r.artifact_id = new.artifact_id and r.event_seq = candidate.seq)
      then
        found_seq := candidate.seq;
      end if;
    end loop;
  end if;
  if found_seq is not null then
    update artifact_reviews set event_seq = found_seq where id = new.id and event_seq is null;
  end if;
  return null;
end $$;
create constraint trigger artifact_reviews_event_seq after insert on artifact_reviews
  deferrable initially deferred
  for each row when (new.event_seq is null)
  execute function artifact_reviews_event_seq();

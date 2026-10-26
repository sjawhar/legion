-- 0117_artifact_reviews_event_seq.up.sql
-- The sequence of the artifact.approved or artifact.changes_requested event each review appended on
-- its document's owner (its issue, else the document). The owner's row lock orders those events as
-- they commit, so the sequence places a review among its issue's other events where created_at, the
-- transaction's start, cannot: a review whose transaction began before another event's and committed
-- after it has the later sequence and the earlier time. The approval read returns it as `seq`, which
-- the Legion daemon orders a review against the start of a root's wait by. Null for a review recorded
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
-- function reads the owner's events newest first while they carry this transaction's now() (the
-- events.created_at default, which no writer sets) and takes the oldest one of the review's type,
-- document and version that no review of the document holds yet, so two reviews written in one
-- transaction take their events in the order they appended them. created_at, not xmin, ties a row
-- to this transaction: a write in a savepoint stamps a subtransaction's xid, which is not the
-- transaction's own. A row this release's docs.WriteReview has already given its sequence is left
-- as it is, and a row whose transaction appended no such event (a test's bare insert) stays null.
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
      select e.seq, e.type, e.payload, e.created_at from events e where e.issue_key = owner_issue order by e.seq desc
    loop
      exit when candidate.created_at <> now();
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
      select e.seq, e.type, e.payload, e.created_at from events e where e.artifact_id = new.artifact_id order by e.seq desc
    loop
      exit when candidate.created_at <> now();
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

-- The outbox reads its batch through three partial indexes and writes an event once,
-- when it records what the event came to (outbox.readBatch, outbox.recordBatch). One process
-- publishes at a time (the dispatch-outbox hold, migration 0150), so no row is claimed any more:
-- next_attempt_at is only a back-off, or the back-off of the earlier event a later one waits on.
--
-- Every predicate spells "not a pending subscription command" the one way the outbox's queries
-- spell it, (payload ->> 'pending') is distinct from 'true', because Postgres uses a partial index
-- only for a query whose own clauses prove the index's predicate, and it proves an expression
-- like this one only from the same expression. A pending command (api/subscribers.go) is never
-- published, so none of these indexes holds one.
--
-- events_outbox_fresh: the unpublished events never tried, in id order.
-- events_outbox_retry: the unpublished events waiting on a back-off, by when it falls due.
-- events_outbox_owner: each owner's unpublished events in id order, the owner being the issue,
-- the document or the project an event belongs to, as the outbox orders their sends.
--
-- publish_error is why the event's last attempt failed, and is cleared when it is published.
--
-- events_unpublished (0003) and events_unpublished_ready (0020) were read by the per-row claim
-- alone, and held every pending command for good; nothing reads them now.
--
-- The migration runs in one transaction, so every lock it takes is held until it commits. The
-- three index builds come first: each reads every row of events under SHARE, which holds writes to
-- events while it runs (no `concurrently`) and lets reads go on. The column and the two drops come
-- last, because each takes ACCESS EXCLUSIVE on events, which holds reads too: reads of events wait
-- only for those last statements, which rewrite no row, and for the commit. The release before
-- this one reads neither the new column nor the new indexes, and its claim, which read the two
-- dropped indexes, still runs without them, at a cost: its predicate
-- coalesce(payload ->> 'pending', 'false') <> 'true' proves none of these indexes' predicates, so
-- each of its scans reads all of events in a sequential scan (125 ms on 1,000,000 rows, against
-- 3.4 ms through events_unpublished). It pays that while it serves beside this release in a
-- rolling update, one more reason to deploy this release with `strategy: Recreate`
-- (cmd/dispatch/README.md).
create index events_outbox_fresh on events (id)
  where published_at is null and next_attempt_at is null and (payload ->> 'pending') is distinct from 'true';

create index events_outbox_retry on events (next_attempt_at, id)
  where published_at is null and next_attempt_at is not null and (payload ->> 'pending') is distinct from 'true';

create index events_outbox_owner on events (coalesce(issue_key, artifact_id::text, project_key), id)
  where published_at is null and (payload ->> 'pending') is distinct from 'true';

alter table events add column publish_error text;

drop index events_unpublished;
drop index events_unpublished_ready;

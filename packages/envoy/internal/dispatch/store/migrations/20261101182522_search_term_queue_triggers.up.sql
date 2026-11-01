-- 20261101182522_search_term_queue_triggers.up.sql
-- The five triggers that fill 20261101182508's search_term_queue: one for each kind of item the
-- related-terms refresh counts.
--
-- Each fires only when an item's search text changes, on the columns that item's search vector is
-- built from (0057-0061) and on the vector itself: issues on key, title and search, comments on
-- body and search, asks on question, options, answer and search, messages on body and search. The
-- event broker updates issues.last_seq and artifacts.last_seq on every event, and neither column
-- is in a list here, so an event append queues nothing. Inserts and deletes carry no column list,
-- because every one of them changes what the project holds.
--
-- search is in each list for the reason 20261101182454 gives for artifact_versions: a write that
-- sets the vector directly is a change to what the project holds, and nothing else would see it.
-- Before this release a full rebuild every ten minutes repaired such a write; now a project that
-- has a snapshot is counted by step passes alone, so a write no trigger sees stays counted wrong
-- with no limit. Postgres fires a column-list trigger on the columns the UPDATE command names,
-- so an ordinary title or body edit, which does not name search, still fires the trigger exactly
-- once.
--
-- A document's changes are taken from 20261101182447's document_search, not from
-- artifact_versions. That table already holds exactly the vector of the version the refresh
-- counts, and 20261101182454's trigger writes it inside the artifact_versions write, so this
-- trigger fires nested inside that one and the refresh needs no second trigger on the 5.2 GiB
-- table. 20261101182454's guard, `where excluded.number >= document_search.number`, correctly
-- queues nothing when a late write of an older version updates no row.
--
-- Each trigger is one insert and reads no row: the spec's bound on what the writes may cost
-- (within 10%) is what keeps it to that, and every value it writes is already in the row the
-- trigger was handed. old_search is the vector the item held before, which is what a pass
-- subtracts; it is null for an insert, and the empty vector where the item's own column was null.
--
-- Five triggers in one migration, in a fixed order - asks, comments, issues, messages, then
-- document_search - so that one transaction takes the five locks in one window instead of five
-- migrations taking one each, and always in that order, so two of these migrations could not
-- deadlock against each other. They can still deadlock with a live writer, which is what the
-- lock_timeout below is for: this transaction holds each table's lock while it waits for the
-- next, and a writer that writes two of these tables in the other order closes a cycle with it.
-- 0066 names two such writers on comments and asks - accepting a suggestion, and a document edit
-- under a comment's anchor - and a version write closes the same cycle on document_search and
-- issues: the insert into artifact_versions fires 20261101182454's trigger, which takes ROW
-- EXCLUSIVE on document_search, and recordTaskProgress (docs/task_progress.go) then updates
-- issues, which takes ROW EXCLUSIVE there even when it matches no row.
--
-- Under the runner's five-second bound either side can lose that race, and the runner retries
-- only 55P03 (store/store.go RetryLockTimeouts), so a deadlock either answers a request 500 or
-- fails the boot. Half a second, shorter than deadlock_timeout (1 s by default), makes this
-- migration give up first instead, before either deadlock check runs: it fails the boot with
-- 55P03 having applied nothing, the writer finishes, and the next boot applies it. This is 0074's
-- rule - never hold one table's lock while waiting for another's - kept the way 0066 keeps it.
--
-- The pre-deploy census exempts the migration from its table-size limit because every statement
-- in it is a create function or a create trigger
-- (internal/pgmigrate/exempt.go tableSizeExemption), and the exemption is per migration, not per
-- table; the four item tables are small (the census of 2026-10-30 gives comments 110 MiB and
-- messages 20 MiB), and document_search is this release's own table. `create trigger` takes
-- SHARE ROW EXCLUSIVE on its table: a read is not held, every write of that table waits for the
-- commit, and the migration waits for the writes already in flight, bounded by
-- pgmigrate.LockTimeout.
--
-- The functions come first and the triggers after, because creating a plpgsql function writes
-- pg_proc and locks no table, so the lock window is the five create trigger statements alone.
set local lock_timeout = '500ms';

create or replace function asks_search_term_queue() returns trigger language plpgsql as $$
declare
  previous tsvector;
begin
  if tg_op <> 'INSERT' then previous := coalesce(old.search, ''::tsvector); end if;
  if tg_op = 'DELETE' then
    insert into search_term_queue (kind, item_id, issue_key, artifact_id, old_search)
    values ('ask', old.id, old.issue_key, coalesce(old.artifact_id, old.block_artifact_id), previous);
  else
    insert into search_term_queue (kind, item_id, issue_key, artifact_id, old_search)
    values ('ask', new.id, new.issue_key, coalesce(new.artifact_id, new.block_artifact_id), previous);
  end if;
  return null;
end $$;

create or replace function comments_search_term_queue() returns trigger language plpgsql as $$
declare
  previous tsvector;
begin
  if tg_op <> 'INSERT' then previous := coalesce(old.search, ''::tsvector); end if;
  if tg_op = 'DELETE' then
    insert into search_term_queue (kind, item_id, issue_key, artifact_id, old_search)
    values ('comment', old.id, old.issue_key, old.artifact_id, previous);
  else
    insert into search_term_queue (kind, item_id, issue_key, artifact_id, old_search)
    values ('comment', new.id, new.issue_key, new.artifact_id, previous);
  end if;
  return null;
end $$;

create or replace function issues_search_term_queue() returns trigger language plpgsql as $$
declare
  previous tsvector;
begin
  if tg_op <> 'INSERT' then previous := coalesce(old.search, ''::tsvector); end if;
  if tg_op = 'DELETE' then
    insert into search_term_queue (kind, issue_key, old_search) values ('issue', old.key, previous);
  else
    insert into search_term_queue (kind, issue_key, old_search) values ('issue', new.key, previous);
  end if;
  return null;
end $$;

create or replace function messages_search_term_queue() returns trigger language plpgsql as $$
declare
  previous tsvector;
begin
  if tg_op <> 'INSERT' then previous := coalesce(old.search, ''::tsvector); end if;
  if tg_op = 'DELETE' then
    insert into search_term_queue (kind, item_id, issue_key, old_search)
    values ('message', old.id, old.issue_key, previous);
  else
    insert into search_term_queue (kind, item_id, issue_key, old_search)
    values ('message', new.id, new.issue_key, previous);
  end if;
  return null;
end $$;

-- document_search has no delete path (20261101182447), so this one covers the two writes
-- 20261101182454's trigger makes: the document's first row, which queues no old vector, and
-- every later replacement of it, which queues the vector the row held.
create or replace function document_search_term_queue() returns trigger language plpgsql as $$
declare
  previous tsvector;
begin
  if tg_op <> 'INSERT' then previous := coalesce(old.search, ''::tsvector); end if;
  insert into search_term_queue (kind, artifact_id, old_search)
  values ('document', new.artifact_id, previous);
  return null;
end $$;

create trigger asks_search_term_queue after insert or delete or update of question, options, answer, search on asks
  for each row execute function asks_search_term_queue();
create trigger comments_search_term_queue after insert or delete or update of body, search on comments
  for each row execute function comments_search_term_queue();
create trigger issues_search_term_queue after insert or delete or update of key, title, search on issues
  for each row execute function issues_search_term_queue();
create trigger messages_search_term_queue after insert or delete or update of body, search on messages
  for each row execute function messages_search_term_queue();
create trigger document_search_term_queue after insert or update of search on document_search
  for each row execute function document_search_term_queue();

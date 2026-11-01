-- 0153: the cap on a document's queued conditional edits holds across envoy-dispatch processes.
-- Each process queues a document's conditional edits in memory, one at a time, and
-- refuses one past 32 (429 EDIT_QUEUE_FULL); with that count in memory alone, two processes
-- together admitted 64.
--
-- A row is one process's count of the conditional edits of one document it has admitted and not
-- yet released, written as an absolute number: queued is never incremented in SQL. A process
-- admits an edit in a short transaction of its own, before the request's transaction, under the
-- two-key advisory lock (docs.conditionalEditQueueLockNamespace, hashtext of the artifact id): it
-- sums the rows of the processes that hold a room-sync connection right now (the members), deletes
-- the rows of every other process, refuses at 32 and otherwise writes its own count. Its release
-- writes its count again, without that lock, and deletes its row when the count reaches zero. A
-- process that dies leaves its row behind; that row stops counting the moment its connection
-- drops, and the next admission deletes it. The release before this one neither reads nor writes
-- the table, and its own queue stays capped per process, as before.
--
-- The migration adds this table and nothing else, so the release before this one runs over the
-- new schema unchanged and the census answers 0. It takes ACCESS EXCLUSIVE on the new table,
-- which nothing else reads yet, and SHARE ROW EXCLUSIVE on artifacts for the foreign key to it
-- (on delete cascade): a write to artifacts waits behind the migration's short transaction, and
-- a read does not.
create table doc_conditional_edit_queue (
  artifact_id uuid not null references artifacts(id) on delete cascade,
  process_id uuid not null,
  queued int not null check (queued > 0),
  primary key (artifact_id, process_id)
);

-- 20261102233629_drop_ask_embeddings_trigger.up.sql
-- An ask is embedded in windows of its parts (20261102233625_block_embedding_windows), which
-- refs.IndexAskParts writes in the transaction that writes the ask's part rows, so the trigger
-- 0076 added, which enqueued the question and the answer's text as one text, has nothing left
-- to do. A window of parts also carries what that text left out: each option, under the
-- question that gives it its meaning.
--
-- Its own migration, so the transaction holds asks' ACCESS EXCLUSIVE lock alone, for
-- milliseconds, and never while waiting for another table's, exactly as 0076 held it alone to
-- add the trigger.
--
-- The rows the dropped trigger wrote stay as they are. The release before this one reads them
-- while it serves, and this release's block index (docs retireWholeText) deletes each one whose
-- owner holds a window whose vector is current.
drop trigger asks_embeddings_enqueue on asks;
drop function embeddings_enqueue_ask();

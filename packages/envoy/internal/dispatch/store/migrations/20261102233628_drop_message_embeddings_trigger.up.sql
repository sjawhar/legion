-- 20261102233628_drop_message_embeddings_trigger.up.sql
-- A message is embedded in windows of its blocks (20261102233625_block_embedding_windows),
-- which refs.IndexItem writes in the transaction that writes the message's block rows, so the
-- trigger 0077 added, which enqueued the whole body as one text, has nothing left to do.
--
-- Its own migration, so the transaction holds messages' ACCESS EXCLUSIVE lock alone, for
-- milliseconds, and never while waiting for another table's, exactly as 0077 held it alone to
-- add the trigger.
--
-- The rows the dropped trigger wrote stay as they are. The release before this one reads them
-- while it serves, and this release's block index (docs retireWholeText) deletes each one whose
-- owner holds a window whose vector is current.
drop trigger messages_embeddings_enqueue on messages;
drop function embeddings_enqueue_message();

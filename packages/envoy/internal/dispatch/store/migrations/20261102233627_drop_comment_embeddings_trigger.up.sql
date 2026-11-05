-- 20261102233627_drop_comment_embeddings_trigger.up.sql
-- A comment is embedded in windows of its blocks (20261102233625_block_embedding_windows),
-- which refs.IndexItem writes in the transaction that writes the comment's block rows, so the
-- trigger 0075 added, which enqueued the whole body as one text, has nothing left to do.
--
-- Its own migration, so the transaction holds comments' ACCESS EXCLUSIVE lock alone, for
-- milliseconds, and never while waiting for another table's, exactly as 0075 held it alone to
-- add the trigger.
--
-- The rows the dropped trigger wrote stay as they are. The release before this one reads them
-- while it serves, and this release's block index (docs retireWholeText) deletes each one whose
-- owner holds a window whose vector is current.
drop trigger comments_embeddings_enqueue on comments;
drop function embeddings_enqueue_comment();

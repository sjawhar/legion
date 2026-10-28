-- 0135_comment_deliveries_pending.up.sql
--
-- The comment mention sender's stranded pass (api/comment_mention_sender.go,
-- strandedCommentMentions) runs in every Dispatch process at start and every 30 s, reading the
-- pending attempts created between a claim lease and an hour ago. comment_deliveries holds one row
-- per mention attempt ever made and its only index is its primary key, so without this index each
-- pass scanned the whole table. Over the pending rows alone, ordered by created_at, the pass reads
-- an empty index when nothing is pending, and only the rows of its window otherwise; a settled
-- attempt leaves the index.
create index comment_deliveries_pending on comment_deliveries (created_at) where state = 'pending';

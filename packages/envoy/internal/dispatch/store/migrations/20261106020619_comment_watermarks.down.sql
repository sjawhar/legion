-- 20261106020619_comment_watermarks.down.sql
--
-- The rollback an operator runs by hand: the triggers first, so nothing writes the counters while
-- they go, then the functions and the table. A build that reads the counters answers every comment
-- list from the validator a missing table cannot give, so this is run only with that build gone.
drop trigger comment_deliveries_watermark on comment_deliveries;
drop trigger comment_mentions_watermark on comment_mentions;
drop trigger comments_watermark on comments;
drop function comment_side_table_watermark();
drop function comments_watermark();
drop function comment_watermark_bump(text, uuid);
drop table comment_watermarks;

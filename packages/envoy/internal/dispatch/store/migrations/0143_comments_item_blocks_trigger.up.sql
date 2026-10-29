-- 0143_comments_item_blocks_trigger.up.sql
--
-- A comment any build writes, on insert and on an edit of its body, is owed a block index
-- (0142's comment_block_state.owed_at). A block-aware build indexes the comment later in the same
-- transaction, which clears owed_at, so a comment still owed was written by a build without the
-- index, during the roll or after a rollback, and the server's block index reconciliation drains
-- it.
--
-- Then every comment already stored is owed too. The seed runs after the trigger, in the same
-- transaction: the trigger holds comments SHARE ROW EXCLUSIVE until the commit, so no comment is
-- written between the two and none is left without a state row. Its own migration, so the
-- transaction holds comments' lock alone (0123's precedent).
create trigger comments_item_blocks_owed after insert or update of body on comments
  for each row execute function item_blocks_owe('comment');

insert into comment_block_state (comment_id, owed_at)
select id, now() from comments
on conflict do nothing;

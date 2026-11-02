-- 0144_messages_item_blocks_trigger.up.sql
--
-- A message any build inserts, a direct message included, is owed a block index (0142's
-- message_block_state.owed_at), as 0143 owes a comment. A message is never edited, so only an
-- insert owes it.
--
-- Then every message already stored is owed too, in the same transaction, after the trigger:
-- the trigger holds messages SHARE ROW EXCLUSIVE until the commit, so no message is written
-- between the two. Its own migration, so the transaction holds messages' lock alone (0125's
-- precedent).
create trigger messages_item_blocks_owed after insert on messages
  for each row execute function item_blocks_owe('message');

insert into message_block_state (message_id, owed_at)
select id, now() from messages
on conflict do nothing;

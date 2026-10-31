-- 0151_asks_block_parts_trigger.up.sql
--
-- An ask any build writes is owed a block index of its parts (0150's ask_block_state.owed_at), as
-- 0143 owes a comment: on insert and on a write of any of the three columns the parts are read
-- from, its question, its options and its answer. A block-aware build indexes the ask later in
-- the same transaction, which clears owed_at, so an ask still owed was written by a build without
-- the index, during the roll or after a rollback, and the server's block index reconciliation
-- drains it.
--
-- 0142's function gains the ask's branch; it keeps its invariant, that a write landing on an
-- existing state row moves only its owed_at, never its revision or next_block, so a drain that
-- read the earlier time leaves the row owed.
--
-- Then every ask already stored is owed too. The seed runs after the trigger, in the same
-- transaction: the trigger holds asks SHARE ROW EXCLUSIVE until the commit, so no ask is written
-- between the two and none is left without a state row. Its own migration, so the transaction
-- holds asks' lock alone (0143's and 0144's precedent).
create or replace function item_blocks_owe() returns trigger language plpgsql as $$
begin
  if tg_argv[0] = 'comment' then
    insert into comment_block_state (comment_id, owed_at) values (new.id, now())
    on conflict (comment_id) do update set owed_at = excluded.owed_at;
  elsif tg_argv[0] = 'message' then
    insert into message_block_state (message_id, owed_at) values (new.id, now())
    on conflict (message_id) do update set owed_at = excluded.owed_at;
  elsif tg_argv[0] = 'ask' then
    insert into ask_block_state (ask_id, owed_at) values (new.id, now())
    on conflict (ask_id) do update set owed_at = excluded.owed_at;
  else
    raise exception 'item_blocks_owe: unknown item kind %', tg_argv[0];
  end if;
  return null;
end;
$$;

create trigger asks_item_blocks_owed after insert or update of question, options, answer on asks
  for each row execute function item_blocks_owe('ask');

insert into ask_block_state (ask_id, owed_at)
select id, now() from asks
on conflict do nothing;

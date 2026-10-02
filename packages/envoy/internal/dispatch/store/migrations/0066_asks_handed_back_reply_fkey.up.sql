-- 0066_asks_handed_back_reply_fkey.up.sql
-- asks.handed_back_reply_id (0064) names a comment: the reply a hand-back answered.
--
-- The key is added not valid, then validated. Adding it takes SHARE ROW EXCLUSIVE on asks and on
-- comments, which waits behind a write to either table but not behind a read, so a comment write
-- that goes on to read asks finishes rather than deadlocking with this migration, as it did when
-- 0064 held asks ACCESS EXCLUSIVE and added the key there. A transaction that writes comments and
-- then writes asks (accepting a suggestion whose version moves the approval request) can still meet
-- it: the deadlock fails this migration, and the next boot applies it again. Validating reads asks
-- for a value comments lacks; every row is null here, since only a binary that has applied this
-- migration writes the column.
alter table asks
    add constraint asks_handed_back_reply_id_fkey foreign key (handed_back_reply_id) references comments(id) not valid;

alter table asks validate constraint asks_handed_back_reply_id_fkey;

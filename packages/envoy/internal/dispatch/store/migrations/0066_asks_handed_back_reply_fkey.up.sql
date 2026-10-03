-- 0066_asks_handed_back_reply_fkey.up.sql
-- asks.handed_back_reply_id (0064) names a comment: the reply a hand-back answered.
--
-- Adding a foreign key takes SHARE ROW EXCLUSIVE on both tables, asks and comments, which waits
-- behind a write to either table but not behind a read. The key is a migration of its own because
-- 0064 holds asks ACCESS EXCLUSIVE, which a read of asks waits behind: added there, it held that
-- lock while it waited for comments, and a comment write that went on to read asks deadlocked with
-- it. Every row is null here, since only a binary that has applied this migration writes the
-- column, so the key is validated as it is added.
--
-- A transaction that writes comments and then writes asks (accepting a suggestion, or a document
-- edit under a comment's anchor, whose version moves the approval request) can still close a cycle
-- with this migration while it waits for comments. Under the runner's five-second bound either
-- side can then lose: Postgres checks a session for a deadlock once, deadlock_timeout (1 s by
-- default) after it starts to wait, so a write that closes the cycle within that second fails this
-- migration, and one that closes it later fails itself, answered 500. Half a second, shorter than
-- deadlock_timeout, makes this migration give up first instead, before either check runs: it fails
-- the boot with 55P03, applying nothing, the comment write finishes, and the next boot applies it.
-- The same bound fails the boot when any comment write holds comments for longer than that.
set local lock_timeout = '500ms';

alter table asks
    add constraint asks_handed_back_reply_id_fkey foreign key (handed_back_reply_id) references comments(id);

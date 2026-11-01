-- 0066 adds a foreign key on asks.handed_back_reply_id, the column 0064 adds null on every row in
-- the same release; only a binary that has applied 0066 writes it, so no existing row holds a value
-- the key could refuse and none is rewritten. The census cannot count it directly, since the column
-- does not exist until 0064 applies. The locks it takes, SHARE ROW EXCLUSIVE on asks and comments,
-- are reported with those tables' row counts and sizes. The census judges their holders against
-- 0066's own 500 ms lock_timeout, so an autovacuum on either table refuses it where
-- deadlock_timeout is not shorter: Postgres would cancel that vacuum only after 0066 gave up.
select 0;

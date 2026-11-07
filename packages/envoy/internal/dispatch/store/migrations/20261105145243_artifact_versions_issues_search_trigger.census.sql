-- 20261105145243 creates one function and one trigger, and nothing else. Its one lock is the
-- SHARE ROW EXCLUSIVE `create trigger` takes on the trigger's table until the commit: writes of
-- that table wait for the instant the transaction lasts, reads do not. Neither statement reads a
-- row, so the migration is exempt from the census's table-size limit, and the trigger runs only on
-- versions written after it: it refuses and rewrites no row already stored. 20261105145242, in the
-- same release, re-indexes the issues stored before it.
select 0;

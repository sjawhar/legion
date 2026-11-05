-- 20261105093500 adds one nullable column with no default to user_threads: no row that exists
-- before it can violate anything it adds, no row is rewritten, and the ACCESS EXCLUSIVE lock it
-- takes on user_threads for the catalogue write is released with the migration's own transaction.
select 0;

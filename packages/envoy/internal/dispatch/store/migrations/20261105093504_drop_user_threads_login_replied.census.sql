-- 20261105093504 drops one index of user_threads and nothing else: no row that exists before it
-- can violate anything it changes, no row is rewritten, and the ACCESS EXCLUSIVE lock it takes on
-- user_threads is released with the migration's own transaction.
select 0;

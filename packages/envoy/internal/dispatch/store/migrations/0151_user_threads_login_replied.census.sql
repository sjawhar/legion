-- 0151 adds one index to user_threads and nothing else: no row that exists before it can violate
-- anything it adds, no row is rewritten, and the SHARE lock it takes on user_threads is released
-- with the migration's own transaction.
select 0;

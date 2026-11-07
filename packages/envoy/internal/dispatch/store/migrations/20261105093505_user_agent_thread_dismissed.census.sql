-- 20261105093505 creates one table and its index and nothing else: the table holds no row until
-- this migration commits, so no row that exists before it can violate anything it adds, no row is
-- rewritten, no existing table is locked, and every lock it takes is released with the migration's
-- own transaction.
select 0;

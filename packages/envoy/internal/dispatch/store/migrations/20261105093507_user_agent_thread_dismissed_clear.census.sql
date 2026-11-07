-- 20261105093507 creates one function and attaches one trigger: no row that exists before it can
-- violate anything it adds, no row is read or rewritten, and the SHARE ROW EXCLUSIVE lock the
-- trigger's catalogue write takes on messages is released with the migration's own transaction.
select 0;

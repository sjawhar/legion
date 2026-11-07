-- 20261105093500 adds two columns to user_threads: dismissed_at, nullable with no default, and
-- version, not null with a constant default, which in this Postgres is the same catalogue write
-- as the other one - the default is read out of the catalogue for a row written before it. So no
-- row that exists before this migration can violate anything it adds, no row is rewritten, and
-- the ACCESS EXCLUSIVE lock it takes on user_threads for that write is released with the
-- migration's own transaction.
select 0;

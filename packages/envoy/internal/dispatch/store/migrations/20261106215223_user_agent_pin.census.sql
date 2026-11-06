-- A new table, which holds no row at this schema: the migration refuses nothing and rewrites
-- nothing, and it reads, locks and writes no table that already exists. It cannot name the new
-- table, which does not exist when the census is taken.
select 0;

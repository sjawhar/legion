-- 20261105204141 adds one column to projects with a constant default, so every existing row takes
-- that default without being rewritten and the column's check, which the empty array satisfies,
-- refuses none of them. It cannot name the column, which does not exist when the census is taken.
select 0;

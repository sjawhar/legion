-- 20261105075455 adds one nullable column to repo_projects with no default, so every stored row
-- takes the null without being rewritten and the column's check, which the null satisfies,
-- refuses none of them. It cannot name the column, which does not exist when the census is taken.
select 0;

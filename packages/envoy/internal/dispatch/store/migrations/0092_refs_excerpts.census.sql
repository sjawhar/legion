-- 0092 adds three nullable columns to refs with no default and a partial index over the rows whose
-- excerpt_version is null (every row, the first time); it rewrites and refuses no row.
select 0;

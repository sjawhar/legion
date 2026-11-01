-- 20261101182515 adds two nullable columns with no default to search_term_totals, which is a
-- catalog write: it reads no row, rewrites none and refuses none. Every row already there reads
-- as the null snapshot of a project this release has not counted yet.
select 0;

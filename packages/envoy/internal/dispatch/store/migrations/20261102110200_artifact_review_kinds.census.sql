-- A new not-null column with a constant default its check admits: every stored review takes
-- 'person' from the catalog, so nothing is refused and no row is rewritten. The census cannot
-- name the column, which does not exist yet when it is taken.
select 0;

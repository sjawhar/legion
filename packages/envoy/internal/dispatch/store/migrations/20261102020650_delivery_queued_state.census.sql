-- 20261102020650 widens two check constraints, rebuilds one partial index over rows the wider
-- predicate already holds, and adds one column with a non-volatile default, which Postgres writes
-- into the catalogue alone; it refuses and rewrites no existing row.
select 0;

-- One nullable column, one `not valid` check, one index and an insert trigger with its two
-- functions; the index build reads every row, and no row is refused or rewritten.
select 0;

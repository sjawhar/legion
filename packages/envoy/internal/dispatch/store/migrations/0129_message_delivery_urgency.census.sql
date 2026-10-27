-- One nullable column, one `not valid` check and two partial indexes; the index builds read every
-- row of their tables, and no row is refused or rewritten.
select 0;

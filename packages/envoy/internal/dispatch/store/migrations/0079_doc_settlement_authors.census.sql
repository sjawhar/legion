-- 0079 adds one non-null JSONB column with a constant default, and one new, empty table.
-- PostgreSQL backfills the column on existing rows without rejecting or rewriting any stored
-- document data; the new table starts empty regardless.
select 0;

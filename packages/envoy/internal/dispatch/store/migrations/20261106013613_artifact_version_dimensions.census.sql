-- 20261106013613 adds two nullable columns to artifact_versions with no default and no check, so
-- no existing row is read, rewritten or refused. It cannot name the columns, which do not exist
-- when the census is taken.
select 0;

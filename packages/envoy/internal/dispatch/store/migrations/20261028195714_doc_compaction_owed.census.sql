-- 20261028195714 creates doc_compaction_owed and fills it from doc_updates, which it only reads: its one
-- insert writes the table it creates, and it refuses and rewrites no existing row. It cannot name
-- the table, which does not exist yet when the census is taken.
select 0

-- 0154 creates document_search and its GIN index. The table is new, so it holds no row, and the
-- migration reads no other table: it refuses and rewrites no existing row.
select 0;

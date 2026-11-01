-- A new table, document_search_backfill, whose one row this migration inserts: it writes no
-- document_search row, reads no other table, and refuses and rewrites nothing already stored.
-- The fill whose progress that row records runs at boot, in committed batches outside every
-- migration transaction (store.FillDocumentSearch).
select 0;

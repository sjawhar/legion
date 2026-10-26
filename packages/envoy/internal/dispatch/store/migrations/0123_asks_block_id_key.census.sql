-- 0123 creates a function and rebuilds two indexes on asks over a digest of the block id each held
-- before. It rewrites no row, and refuses none: the rows asks_block_id_unique holds unique differ
-- in block_id, so they differ in its SHA-256 too.
select 0

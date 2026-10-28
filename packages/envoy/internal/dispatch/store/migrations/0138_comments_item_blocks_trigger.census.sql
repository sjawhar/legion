-- 0138 adds a trigger on comments and writes only comment_block_state, which 0137 of the same
-- release creates and which holds no row at deploy: it refuses and rewrites no existing row. The
-- rows it inserts are new work owed, not a rewrite.
select 0

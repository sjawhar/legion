-- 0144 adds a trigger on messages and writes only message_block_state, which 0142 of the same
-- release creates and which holds no row at deploy: it refuses and rewrites no existing row. The
-- rows it inserts are new work owed, not a rewrite.
select 0

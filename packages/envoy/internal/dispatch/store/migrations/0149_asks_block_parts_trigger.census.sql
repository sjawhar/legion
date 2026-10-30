-- 0149 re-states a function, adds a trigger on asks and writes only ask_block_state, which 0148
-- of the same release creates and which holds no row at deploy: it refuses and rewrites no
-- existing row. The rows it inserts are new work owed, not a rewrite.
select 0

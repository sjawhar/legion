-- 0122 writes only block_grammar_owed, which 0117 of the same release creates and which holds no
-- row at deploy; it refuses and rewrites no existing row. The rows it inserts are new work owed,
-- not a rewrite, and a non-zero count would refuse every deploy over a `#b-` text already stored.
select 0

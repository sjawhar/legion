-- 20261103205042 writes only refs_grammar_owed, which 20261103205037 of the same release creates
-- and which holds no row at deploy; it refuses and rewrites no existing row. The rows it inserts
-- are new work owed, not a rewrite, and a non-zero count would refuse every deploy over a text
-- already stored.
select 0

-- 0071 creates one function and replaces the bodies of four search triggers' functions (the issues
-- trigger's is 0072's). It names no table, so it locks none above ACCESS SHARE, and it rewrites no
-- row: every stored vector fit Postgres's limit when its trigger wrote it, and for text whose vector
-- fits the new functions build exactly the vector the old ones did, so a row written again keeps the
-- vector it has.
select 0;

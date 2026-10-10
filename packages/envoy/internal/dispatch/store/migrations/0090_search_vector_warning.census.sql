-- 0090 replaces search_vector's body. It names no table, so it locks none above ACCESS SHARE, and
-- it rewrites no row: for text that fits it builds 0087's vector exactly, and only a cut, which
-- happens on a write and never to a stored row, now raises a warning.
select 0;

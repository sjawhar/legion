-- 0137 creates one index on asks over its anchor's artifact id. It rewrites no row, and refuses
-- none: the index is not unique, and every anchored row's artifact id already casts to uuid.
select 0

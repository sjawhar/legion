-- 0159 adds five functions and five triggers, which run only on later writes: it reads no row,
-- refuses none and rewrites none. The queue they fill starts empty, and the first refresh pass of
-- this release rebuilds each project in full, so nothing is owed for the writes made before it.
select 0;

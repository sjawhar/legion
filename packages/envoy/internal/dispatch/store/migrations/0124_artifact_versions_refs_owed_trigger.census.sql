-- 0124 adds a trigger on artifact_versions: it refuses and rewrites no existing row, and only
-- enqueues future re-index work.
select 0

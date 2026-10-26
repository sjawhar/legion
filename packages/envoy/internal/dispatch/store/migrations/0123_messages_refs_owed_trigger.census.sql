-- 0123 adds a trigger on messages: it refuses and rewrites no existing row, and only enqueues
-- future re-index work.
select 0

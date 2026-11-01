-- 0156 adds a function and a trigger on artifact_versions, which run only on later writes: it
-- refuses and rewrites no existing row. 0157 is what fills document_search for the versions
-- already stored.
select 0;

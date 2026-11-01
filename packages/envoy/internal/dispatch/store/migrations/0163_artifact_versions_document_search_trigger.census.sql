-- 0163 adds a function and a trigger on artifact_versions, which run only on later writes: it
-- refuses and rewrites no existing row. The boot's fill, whose progress row 0164 creates
-- (store.FillDocumentSearch), is what covers the versions already stored.
select 0;

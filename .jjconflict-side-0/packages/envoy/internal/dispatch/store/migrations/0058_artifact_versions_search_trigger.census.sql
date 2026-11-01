-- 0058 keeps every stored artifact_versions.search value when it drops the generated expression,
-- then adds a trigger for later writes. It refuses and rewrites no existing row; 0062 owns the
-- re-index.
select 0;

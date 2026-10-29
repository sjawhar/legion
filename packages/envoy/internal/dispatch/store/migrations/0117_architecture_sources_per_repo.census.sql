-- 0117 refuses every component whose project has no architecture source: its new foreign key reads
-- each component's source_id as 0, its project's one existing source, and a project without one
-- has no row for it to name. Only direct SQL makes such a component; the repair, run on the release
-- still serving, is `delete from components c where not exists (select 1 from architecture_sources
-- s where s.project_key = c.project_key)`. Nothing else refuses or is rewritten: every existing
-- source reads id 0 and is still the only source of its project, so (project_key, id) and
-- (project_key, repo) are unique already.
select count(*) from components c
where not exists (select 1 from architecture_sources s where s.project_key = c.project_key);

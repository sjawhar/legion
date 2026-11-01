-- 0113 refuses every issue more than 32 links below its root (parentDepthCap, api/issue_parent.go),
-- or in a parent loop: a release before it let a create build a chain of any depth, and this
-- release neither builds nor reads one. Each issue's walk climbs at most 32 links, and one still
-- going there is counted, as 0113's own check counts it. Nothing else refuses or is rewritten:
-- every mapping's new is_default is false, so the partial unique index holds; repo is
-- repo_projects' primary key, so (repo, project) is already unique; and every issue's new repo is
-- null, which the foreign key does not check.
with recursive walk as (
	select key, parent_key, 0 as depth from issues
	union all
	select w.key, i.parent_key, w.depth + 1
	from walk w join issues i on i.key = w.parent_key
	where w.depth < 32
)
select count(*) from walk where depth = 32 and parent_key is not null;

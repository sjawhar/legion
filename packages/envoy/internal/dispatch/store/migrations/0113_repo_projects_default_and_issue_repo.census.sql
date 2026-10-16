-- 0113 refuses no row and rewrites none: every mapping's new is_default is false, so the partial
-- unique index holds; repo is repo_projects' primary key, so (repo, project) is already unique; and
-- every issue's new repo is null, which the foreign key does not check.
select 0;

-- 0113_repo_projects_default_and_issue_repo.up.sql — a project's default repository and an issue's own.
--
-- A project maps many repositories (0005) and names at most one of them its default, which an issue
-- with no repository of its own and none on its parent chain resolves to. An issue's own repository
-- must be a mapping of its project: the composite foreign key makes that a database invariant, so a
-- mapping an issue names cannot be deleted or moved to another project (23503, which the API answers
-- REPO_PROJECT_IN_USE). Resolution through the parent chain is computed on read
-- (api.issueRepoLateral), as component attachments are (0038).
--
-- An issue sits at most 32 links below its root (parentDepthCap, api/issue_parent.go). A release
-- before this one let a create build a chain of any depth; this one refuses to build one and fails
-- every read of an issue past the cap. So the migration first refuses while any issue sits deeper,
-- or in a parent loop, naming up to 20 of them and the repair, and applies nothing. Its census
-- counts the same issues, so the pre-deploy census refuses first.
--
-- It takes both tables it changes in ACCESS EXCLUSIVE before it counts, and holds them to commit,
-- so no earlier release's write commits between the count and the ALTERs: a create that commits
-- while this waits for the lock is in the count. ACCESS EXCLUSIVE, not EXCLUSIVE, because
-- the ALTER TABLEs below take it anyway, and taking it later would be an upgrade an old server's
-- read-then-write transaction on issues can deadlock with (the read granted beside EXCLUSIVE, the
-- write then queued behind it). repo_projects first, as the earlier release's mapping writes touch
-- it alone and its one read of it outside them holds no transaction. Reads of both tables queue for
-- as long as the count and the ALTERs take; the runner's lock_timeout (pgmigrate.LockTimeout, 5 s)
-- bounds the wait for the lock, and fails the boot past it.
--
-- Only this release's API holds the cap; nothing here does. An earlier release serving once this
-- commits, left running or started again by a rollback, could build a chain past it again, so the
-- earlier release stops before this one boots and a failed deploy recovers forward
-- (cmd/dispatch/README.md, "Database migrations"). A chain built anyway, or by direct SQL, fails the
-- reads of its issues (500) until it is repaired as that README says.
lock table repo_projects, issues in access exclusive mode;

do $$
declare
	deep_count bigint;
	deep_keys text;
begin
	with recursive walk as (
		select key, parent_key, 0 as depth from issues
		union all
		select w.key, i.parent_key, w.depth + 1
		from walk w join issues i on i.key = w.parent_key
		where w.depth < 32
	), deep as (
		select w.key, i.project_key, i.number
		from walk w join issues i on i.key = w.key
		where w.depth = 32 and w.parent_key is not null
	)
	select count(*), (select string_agg(named.key, ', ') from (select key from deep order by project_key, number limit 20) named)
	into deep_count, deep_keys
	from deep;
	if deep_count > 0 then
		raise exception using
			errcode = 'check_violation',
			message = format(
				'0113 refuses: %s issue(s) sit more than 32 links below their root, or in a parent loop: %s%s. '
				'This release refuses a parent chain longer than 32 links and fails every read of an issue past one. '
				'Detach each chain first, on the release running now: PATCH /api/v1/issues/<ancestor> with {"parent": null}, '
				'choosing ancestors that leave no issue more than 32 links below its root; then deploy again.',
				deep_count, deep_keys,
				case when deep_count > 20 then format(' and %s more', deep_count - 20) else '' end);
	end if;
end $$;
alter table repo_projects add column is_default boolean not null default false;
create unique index repo_projects_default_per_project on repo_projects (project) where is_default;
alter table repo_projects add constraint repo_projects_repo_project_key unique (repo, project);
alter table issues add column repo text;
alter table issues add constraint issues_repo_mapped_to_project
  foreign key (repo, project_key) references repo_projects (repo, project);
create index issues_repo on issues (repo) where repo is not null;

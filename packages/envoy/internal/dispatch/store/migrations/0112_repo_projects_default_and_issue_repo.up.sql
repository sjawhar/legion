-- 0112_repo_projects_default_and_issue_repo.up.sql — a project's default repository and an issue's own.
--
-- A project maps many repositories (0005) and names at most one of them its default, which an issue
-- with no repository of its own and none on its parent chain resolves to. An issue's own repository
-- must be a mapping of its project: the composite foreign key makes that a database invariant, so a
-- mapping an issue names cannot be deleted or moved to another project (23503, which the API answers
-- REPO_PROJECT_IN_USE). Resolution through the parent chain is computed on read
-- (api.issueRepoLateral), as component attachments are (0038).
alter table repo_projects add column is_default boolean not null default false;
create unique index repo_projects_default_per_project on repo_projects (project) where is_default;
alter table repo_projects add constraint repo_projects_repo_project_key unique (repo, project);
alter table issues add column repo text;
alter table issues add constraint issues_repo_mapped_to_project
  foreign key (repo, project_key) references repo_projects (repo, project);
create index issues_repo on issues (repo) where repo is not null;

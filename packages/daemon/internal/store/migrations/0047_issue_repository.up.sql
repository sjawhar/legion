-- 0047_issue_repository.up.sql — the repository an issue targets and its default branch.
--
-- Both are taken when the daemon first records the issue (a root's admission, a child's recording
-- under its tree, an orphan's admission) and never change after. Existing rows are filled by the
-- next boot from Dispatch's resolved repository and GitHub's default branch
-- (workflowRuntime.backfillRepositories), which refuses to run the workflow while any row is
-- unfilled; the record store refuses to write an empty one from then on.
alter table issues add column repo text not null default '';
alter table issues add column default_branch text not null default '';

-- What the daemon last said on a Dispatch issue about its repository, so a repeated event posts
-- nothing new: the issue that was not admitted because its repository resolved to none or to one
-- the daemon left out, or whose repo was edited in Dispatch after it was recorded.
create table repo_notes (
  issue text primary key,
  resolved_repo text not null,
  note text not null,
  posted_at timestamptz not null
);

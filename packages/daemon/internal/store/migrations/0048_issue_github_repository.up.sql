-- 0048_issue_github_repository.up.sql — GitHub's spelling of an issue's repository.
--
-- An issue's record keeps its repository in the spelling its workspace and shared clone were made
-- under (repo), which can be a clone's that differs from GitHub's in case. GitHub's own spelling
-- (github_repo) is what Envoy's event subjects carry, which NATS matches case-sensitively, so the
-- daemon subscribes to an issue's repository by it, read from this column at boot rather than from
-- GitHub. It is taken with repo when the daemon first records the issue and never changes after.
-- SQL cannot know it for an existing row, so the next boot reads it from GitHub before it subscribes
-- (workflowRuntime's backfillRepositories); the record store refuses an empty one from then on.
alter table issues add column github_repo text not null default '';

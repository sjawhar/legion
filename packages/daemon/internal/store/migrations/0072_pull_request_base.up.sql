-- 0072_pull_request_base.up.sql — the base branch a pull request's head merges into
-- (record.PullRequest.Base), as GitHub's pull-request webhook names it in `base_ref` and the
-- /pulls/{number} read the daemon already makes names it in `base.ref`.
--
-- The daemon kept no base at all, so a push to a base branch could not be matched to the pull
-- requests sitting on it. A push to a base can take a head GitHub reported clean to behind or
-- dirty, and GitHub sends no event for the pull request itself when it does, so the base push is
-- the only notice the daemon gets, and without the base it could not tell which pull requests that
-- push concerns. The index is exactly the lookup the base push's handler makes (workflow's
-- basePush): the open pull requests of one repository on one base, one indexed read that returns no
-- row for a base nothing is recorded on, which is the common case for a repository other automation
-- pushes to.
--
-- Every existing row takes '', which reads as "not recorded yet" rather than a base the migration
-- would have to invent from a column that does not hold one: the daemon's next read of the pull
-- request fills it, since every such read decodes the base GitHub names, and until then no base
-- push matches the row.
alter table pull_requests add column base text not null default '';
create index pull_requests_open_on_base on pull_requests (repo, base) where state = 'open';

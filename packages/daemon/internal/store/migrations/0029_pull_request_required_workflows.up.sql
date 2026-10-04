-- 0029_pull_request_required_workflows.up.sql — a base branch's rulesets can require a workflow to
-- succeed (a workflows rule) as well as checks, and GitHub merges only once that workflow's run for
-- the head succeeded. The daemon reads each required workflow's latest run on the head with the
-- required set (record.PullRequest.Workflows), and a result stands only for the head it was read
-- at (required_workflows_head).
--
-- Every existing row starts with none: the daemon's required-checks pass reads each open pull
-- request's set and runs as it boots, and a read that finds a required workflow differs from the
-- row, so it is applied and decides the head's verdict again.
alter table pull_requests add column required_workflows jsonb not null default '[]'::jsonb;
alter table pull_requests add column required_workflows_head text not null default '';

-- 0028_pull_request_required_checks.up.sql — the checks a pull request's base branch requires, as
-- the daemon last read them from GitHub (record.PullRequest.Required).
--
-- Only a required check decides whether CI is red at a head, so a pull request whose required set
-- was never read has no checks verdict. Null is that state, and every existing row starts in it:
-- the daemon reads each open pull request's set as it boots and writes it here, which decides each
-- head's verdict again.
alter table pull_requests add column required jsonb;

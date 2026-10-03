-- 0027_drop_pull_request_github_read_columns.up.sql — a pull request drops the three columns only a
-- read of GitHub's checks could set.
--
-- Listener settlements are the daemon's only CI input and webhooks its only pull request
-- lifecycle input; it reads nothing from GitHub's checks. So no row holds anything the daemon
-- would lose: reconciled is false, head_updated_at_source is 'webhook', and failing_statuses holds
-- no failing status.
alter table pull_requests drop column reconciled;
alter table pull_requests drop column head_updated_at_source;
alter table pull_requests drop column failing_statuses;

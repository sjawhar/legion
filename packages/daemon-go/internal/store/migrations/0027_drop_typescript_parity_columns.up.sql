-- 0027_drop_typescript_parity_columns.up.sql — a pull request records only what the daemon's own
-- inputs can set.
--
-- Listener settlements are the daemon's only CI input and webhooks its only pull request
-- lifecycle input; it reads nothing from GitHub's checks. Each column below held a value only a
-- GitHub read could change, so every row holds the one value the daemon writes: reconciled false,
-- head_updated_at_source 'webhook', failing_statuses '[]'.
alter table pull_requests drop column reconciled;
alter table pull_requests drop column head_updated_at_source;
alter table pull_requests drop column failing_statuses;

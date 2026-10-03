-- 0027_drop_pull_request_reconciled.up.sql — a pull request no longer records whether a GitHub
-- read reconciled its CI fence.
--
-- Only a GitHub rollup read could set it, and the daemon makes none: listener settlements are its
-- only CI input, so every row holds false.
alter table pull_requests drop column reconciled;

-- 0028_pull_request_required_checks.up.sql — a pull request's checks are judged by the checks its
-- base branch requires, which the daemon reads from GitHub (record.PullRequest.Required), so the
-- settlement keeps what it names - failed and cancelled checks apart - and no verdict of its own.
--
-- required is null until the daemon reads the set, and every existing row starts so: the daemon
-- reads each open pull request's set as it boots, which decides each head's verdict again.
alter table pull_requests add column required jsonb;
-- cancelled is the settlement's cancelled checks. Rows written before kept none of them, so a
-- check cancelled in a settlement they record reads as passed until the head settles again.
alter table pull_requests add column cancelled jsonb not null default '[]'::jsonb;
-- A checked head now means a settlement was recorded for it (classify's settled). A row with no
-- verdict recorded none it can be judged by: migration 0024 gave every row its own head as checked
-- head whether or not it had settled, and a settlement whose checks all ended cancelled recorded
-- no verdict and no names. Such a row is unsettled until its head's next settlement.
update pull_requests set checked_head = '' where verdict = '';
alter table pull_requests drop column verdict;

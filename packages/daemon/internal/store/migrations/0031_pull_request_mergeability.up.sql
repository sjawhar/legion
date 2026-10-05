-- 0031_pull_request_mergeability.up.sql — GitHub's lazily computed verdict for whether a pull
-- request's head can be merged into its base without a conflict (record.PullRequest.Mergeability),
-- as the daemon's periodic required-checks read of /pulls/{number} last found it. mergeability is
-- '' until the daemon reads it, as every existing row starts: a row never read decides nothing
-- (classify.ConflictWithdrawsReady reads only MERGEABILITY_CONFLICTING). The base branch itself is
-- not kept on the row: the checks-red withdrawal reason that names it reads it off the
-- intake.PullRequestMergeability fact that set this column, never a stored copy.
alter table pull_requests add column mergeability text not null default '';

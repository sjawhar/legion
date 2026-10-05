-- 0031_pull_request_mergeability.up.sql — GitHub's lazily computed verdict for whether a pull
-- request's head can be merged into its base without a conflict (record.PullRequest.Mergeability),
-- as the daemon's periodic required-checks read of /pulls/{number} last found it, and the base
-- branch that read named (record.PullRequest.Base). mergeability is '' until the daemon reads it,
-- as every existing row starts: a row never read decides nothing (classify.ConflictWithdrawsReady
-- reads only MERGEABILITY_CONFLICTING). base is '' until the same read names it.
alter table pull_requests add column mergeability text not null default '';
alter table pull_requests add column base text not null default '';

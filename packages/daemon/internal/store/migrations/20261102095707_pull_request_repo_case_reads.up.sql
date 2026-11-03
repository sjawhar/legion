-- 20261102095707_pull_request_repo_case_reads.up.sql — the indexes the repository-keyed pull
-- request reads use now that each compares the repository name without case.
--
-- The three reads that key pull_requests on its repository write `lower(repo) = lower($1)`
-- (record.Store.PullRequestByBranch, record.Store.PullRequestByNumber, record.PullRequestRecorded),
-- because GitHub spells a repository as its owner does and a configured rename names one repository
-- under two spellings. lower(repo) is neither the expression 0062's pull_requests_repo_number
-- indexes nor the one 0004's unique pull_requests_repo_branch indexes, so neither index bounds
-- these reads any more: the planner walks the whole index and filters every row it reads. 0062 made
-- its index so the GitHub consumer's read of whether a pull request is recorded — one read before
-- any transaction, on every review and every checks settlement of every mapped repository, and
-- twice on a miss when a rename pair is configured — does not grow with the records of finished
-- issues, which are never deleted. These two indexes are that bound again, on the expression the
-- reads now write.
--
-- pull_requests_lower_repo_number replaces 0062's index and serves PullRequestByNumber and
-- PullRequestRecorded. pull_requests_lower_repo_branch serves PullRequestByBranch, the read of the
-- pull request a push's branch names. 0004's unique pull_requests_repo_branch stays: it is the
-- table's uniqueness rule, one row per repository and branch, not a read's index.
drop index pull_requests_repo_number;
create index pull_requests_lower_repo_number on pull_requests (lower(repo), number);
create index pull_requests_lower_repo_branch on pull_requests (lower(repo), branch);

-- 0062_live_issue_reads.up.sql — indexes for the reads a fact makes, so its cost does not grow with
-- the records of finished issues, which are never deleted.
--
-- issues_live serves record.Store.LiveIssues: the project's records not in phase done, and the done
-- ones set back to todo (a root a person re-admits, a parked child), which is every record admission
-- and the waiting line read. Its predicate is the one LiveIssues writes, so the planner uses it.
create index issues_live on issues (project) where phase <> 'done' or status = 'todo';

-- issues_tree serves record.Store.TreeIssues: a tree's members, done ones included, read by tree
-- (the linger, a merge's wait on the rest of its tree, adoption), never the whole project.
create index issues_tree on issues (tree);

-- pull_requests_repo_number serves the GitHub consumer's read, before any transaction, of whether
-- the daemon records the pull request a review or a checks settlement names (intake's
-- recordedPullRequest), and record.Store.PullRequestByNumber.
create index pull_requests_repo_number on pull_requests (repo, number);

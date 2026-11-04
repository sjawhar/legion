-- 20261102122400_default_branch_commits.up.sql
--
-- What Dispatch's GitHub App answered about one commit of one repository: is it on the
-- repository's default branch? A result for an after-merge or default-branch check is current
-- only at such a commit, and the answer is read when the results are read, never written by a
-- push, so this is the cache that keeps those reads off GitHub.
--   repo, sha    the repository ("owner/repo") and the 40-hex commit asked about.
--   on_default   GitHub's answer. A yes is kept for good: a commit on the default branch never
--                leaves it. A no is re-read after five minutes, since a commit merges later.
--   checked_at   when it was read, which is what the five minutes are measured from.
--
-- A new table: nothing existing is read, locked or rewritten, and the release before this one
-- neither reads nor writes it, so it rolls beside this one (cmd/dispatch/README.md, "Database
-- migrations"). The census answers 0, and rolling back past it is an image rollback alone.
create table default_branch_commits (
    repo text not null,
    sha text not null,
    on_default boolean not null,
    checked_at timestamptz not null default now(),
    primary key (repo, sha)
);

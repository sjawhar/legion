-- 0036_pull_request_head_counted_before.up.sql — the fix-attempt mark a count replaced
-- (record.PullRequest.HeadCountedBefore). A head whose push has not arrived yet is counted, and
-- when that push proves handoff-only or the review App's the count is taken back
-- (classify.ApplyPush), restoring this mark rather than clearing it, so the counted attempt the
-- fix-attempt cap holds (classify.FixAttemptCapHolds) is the same whichever of the push and the
-- head's arrival GitHub delivers first. '' on every existing row, which a take-back reads as no
-- attempt standing, as it was read before this column existed.
alter table pull_requests add column head_counted_before text not null default '';

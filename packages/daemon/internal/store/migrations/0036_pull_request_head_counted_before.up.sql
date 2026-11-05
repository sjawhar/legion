-- 0036_pull_request_head_counted_before.up.sql — the fix-attempt mark a count replaced
-- (record.PullRequest.HeadCountedBefore). A head whose push had not arrived yet was counted, and
-- when that push proved handoff-only or the review App's the count was taken back
-- (classify.ApplyPush), restoring this mark rather than clearing it, so the counted attempt was
-- the same whichever of the push and the head's arrival GitHub delivered first. '' on every
-- existing row, which a take-back read as no attempt standing, as it was read before this column
-- existed. The rule that read the count is gone (dispatch://INFRA-956 R4).
alter table pull_requests add column head_counted_before text not null default '';

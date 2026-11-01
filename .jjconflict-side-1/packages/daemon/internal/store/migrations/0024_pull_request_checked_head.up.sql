-- 0024_pull_request_checked_head.up.sql — the head whose CI settlement a pull request's verdict
-- records.
--
-- A handoff push that changes only .legion/ can carry GitHub's skip-checks trailer and start no CI,
-- so the settlement that stands for the head is the code head's, arriving after the handoff head
-- (record.PullRequest.CheckedHead). Every row written before this column recorded the settlement of
-- its own head, since a new head cleared the verdict, so each one's checked head is its head.
alter table pull_requests add column checked_head text not null default '';
update pull_requests set checked_head = head_sha;

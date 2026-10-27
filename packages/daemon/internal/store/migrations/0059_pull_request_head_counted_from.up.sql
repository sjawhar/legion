-- 0059_pull_request_head_counted_from.up.sql — the head a counted fix attempt moved from
-- (record.PullRequest.HeadCountedFrom). A head whose push has not arrived yet is counted, and the
-- count is taken back (classify.ApplyPush) only by the push that made the move the count counted:
-- the one that left the counted head from this head. A push that left the same head from another
-- head (the required-checks pass's compare from an approved head) takes nothing back, since the
-- counted move can carry code that push does not. '' on every existing row: a count made before
-- this column existed names no move, so no push takes it back, and the count stands.
alter table pull_requests add column head_counted_from text not null default '';

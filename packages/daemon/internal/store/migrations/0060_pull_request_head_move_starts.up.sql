-- 0060_pull_request_head_move_starts.up.sql — where a pull request's head moves started, so a move
-- is judged by the chain of recorded pushes it took, whichever of the pushes and the head arrives
-- first.
--
-- head_counted_from (record.PullRequest.HeadCountedFrom) is the head a counted fix attempt moved
-- from. The count is taken back (classify.ApplyPush) once the recorded pushes chain from it to the
-- counted head, each the review App's or an unforced handoff-only push. A push that reached the
-- counted head from another head (the required-checks pass's compare from an approved head) is on
-- no such chain, and takes nothing back: the counted move can carry code that push does not.
--
-- head_arrived_from (record.PullRequest.HeadArrivedFrom) is the head the current head replaced, set
-- on every head move. The planned mark (planned_red) is the last push that may change code on the
-- chain from it to the current head.
--
-- '' on every existing row: a count made before this migration names no start, so nothing takes it
-- back, and the planned mark of a head that arrived before it is settled only by that head's own
-- push, as before.
alter table pull_requests
  add column head_counted_from text not null default '',
  add column head_arrived_from text not null default '';

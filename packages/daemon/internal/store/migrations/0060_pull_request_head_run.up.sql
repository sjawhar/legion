-- 0060_pull_request_head_run.up.sql — the head run (record.PullRequest.Run), which replaces the
-- marks a late push took a count back by: the mark a count replaced (0036) and where a count and a
-- head moved from (0059).
--
-- A head that arrives before every push of its move is recorded opens a run: the pull request's
-- judgement as it stood before that head (the count and its mark, the planned mark, the CI
-- settlement and the required set), the pushes it held, and every head, settlement and required
-- read applied since. Each push recorded while the run is open replays the run from that judgement
-- with every push (classify.ApplyPush), so the count and the planned mark are those the facts give
-- had every push arrived first, in whatever order GitHub delivered them. The run closes once every
-- head in it has its pushes, and is cut to its newest steps when it grows past its bound.
--
-- null on every existing row: no run is open, so a head that arrived before the deploy is judged as
-- the release before judged it, and a late push for it takes nothing back.
alter table pull_requests
  add column head_run jsonb,
  drop column head_counted_before,
  drop column head_counted_from,
  drop column head_arrived_from;

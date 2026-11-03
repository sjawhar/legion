-- 0061_pull_request_head_run.up.sql — the head run (record.PullRequest.Run), which replaces the
-- marks a late push took a count back by: the mark a count replaced (0036) and where a count and a
-- head moved from (0060).
--
-- A head that arrives before every push of its move is recorded opens a run: the pull request's
-- judgement as it stood before that head (the count and its mark, the planned mark, the CI
-- settlement and the required set), the pushes it held, and every head, settlement and required
-- read applied since. Each push recorded while the run is open replays the run from that judgement
-- with every push (classify.ApplyPush), so the count and the planned mark are those the facts give
-- had every push arrived first, in whatever order GitHub delivered them. The run closes once every
-- head in it has its pushes, and is cut to its newest steps when it grows past its bound.
--
-- head_moves (record.PullRequest.Moves) is the moves the record made, the last 32: a push is spent
-- when its own move - its two heads - is among them, so a push that returns the branch to a commit
-- it held before is never spent by the commit's first arrival.
--
-- null head_run and '[]' head_moves on every existing row: no run is open, so a head that arrived
-- before the deploy is judged as the release before judged it, and a late push for it takes nothing
-- back; a late push for a move made before the deploy waits as a push not yet linked, until the
-- bound on such pushes lets it go.
alter table pull_requests
  add column head_run jsonb,
  add column head_moves jsonb not null default '[]',
  drop column head_counted_before,
  drop column head_counted_from,
  drop column head_arrived_from;

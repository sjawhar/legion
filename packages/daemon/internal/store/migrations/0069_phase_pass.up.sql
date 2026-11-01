-- 0069_phase_pass.up.sql — pass (record.PhaseRow.Pass), the count of the moves an issue has made
-- back into an earlier phase, which rounds counted until now beside the review rounds it bounds.
--
-- The two counts are one fact apart. A pass is one run of the implementing, testing and reviewing
-- stretch: every backward move opens one, a tester's fail, a review that requested changes and a
-- head that conflicts with its base among them. The handoff completion key carries it
-- (internal/api/handoff.go), which is the whole of its job: a completion reporting the commit the
-- pass before carried takes a key of its own, so the workflow refuses it as a handoff not written
-- anew instead of the route answering it as already received. rounds keeps the other job, the one
-- review_round_cap is a cap on, and so moves only on a review that requested changes.
--
-- Every existing row takes its rounds as its pass: rounds was the completion key's counter before
-- this split, so no live issue's key changes meaning across the deploy.
alter table phases add column pass integer not null default 0;
update phases set pass = rounds;

-- 0067_phase_assignment.up.sql — the assignment a completion belongs to.
--
-- A completion is attributed to a run today (0012's serving_generation), and every phase entry of
-- one run carries that same run, so a turn that outlived its phase could complete the next entry
-- of the same phase: a tester's turn from the first testing phase, still running while CI sent the
-- work back to implementing and the implementer's round sent it to testing again, reports a
-- completion the daemon would take for the new testing phase's. The assignment is the finer fence:
-- a token the workflow mints at each start that hands a role a task, written in the transition's
-- own transaction onto the role's phases row and onto the start's payload, and carried from there
-- to the delivery and, when a confirmed delivery is retired, onto the claim. The completion route
-- compares the two (HANDOFF_STALE_ASSIGNMENT).
--
-- phases.assignment is what the issue's current phase handed the role; the claim's
-- serving_assignment is what the claim's worker took, as serving_generation is the run it took;
-- the delivery's carries the value from the start row to that retire.
--
-- No backfill, unlike 0012's. A daemon upgraded in place has workers between turns and workers
-- mid-turn, and both sides of every one of them is empty, which the route reads as nothing to
-- compare and accepts; the first start each role is given after the upgrade stamps both sides at
-- once, in one transaction, so no live worker's completion is refused for a value only one side
-- has. record.ClearGeneration clears the column with the rest of the run's record.
alter table phases add column assignment text not null default '';
alter table pending_task_deliveries add column assignment text not null default '';
alter table claims add column serving_assignment text not null default '';

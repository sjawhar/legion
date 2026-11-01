-- 0079_claim_death_and_start_causes.up.sql — why a claim's process last died, why its last start
-- failed, and when its agent was last ready: the three answers an operator and the workflow need
-- to say what happened to a worker that is not running.
--
-- The daemon already learns all three. A death carries the runtime's own account of it
-- (runtime.Observation.Detail: an evicted pod, a node that went away, an init container that
-- failed), a start that fails carries the error that failed it, and a ready is the transition the
-- machine makes. None was kept, so each lived only in the daemon's log: a root architect died six
-- times in 54 minutes and neither its claim, its notices nor its own relaunched session ever said
-- why (dispatch://INFRA-714 R6).
--
-- The two causes are separate columns because they answer separate questions: a claim that cannot
-- start and a claim that keeps dying are different faults with different fixes, and one must never
-- overwrite the other. Each keeps its last value until another of its own kind replaces it, and
-- last_ready is what dates them: a cause older than the ready beside it is one the running agent
-- has already survived.
--
-- Every existing row takes the empty string for the causes, which is "none recorded", and null for
-- the ready, which no claim that ran before this release wrote; a backfill would have to invent
-- times and sentences the daemon does not hold.
alter table claims add column last_death_cause text not null default '';
alter table claims add column last_start_failure text not null default '';
alter table claims add column last_ready timestamptz;

-- A pending task also records whether the daemon has put a prompt of it to the agent at all. A
-- task that was sent and came to nothing — refused, answered busy, acknowledged and never turned
-- into a turn — already loses its read mark, but a cleared mark cannot be told from a task the
-- daemon has not sent yet, and the two mean opposite things for a claim that has served no run:
-- the task it has not been given is the run it is working, and the task it refused is not
-- (supervise's Claim.ServingRun). Existing rows take false, which costs at most one needless
-- credit on a task already in flight at the deploy and nothing after that.
alter table pending_task_deliveries add column prompted boolean not null default false;

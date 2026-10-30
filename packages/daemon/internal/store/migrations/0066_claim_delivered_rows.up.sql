-- 0066_claim_delivered_rows.up.sql — every outbox start a claim queued a task from.
--
-- A start row delivers its task to the claim (supervise's queue) and then finishes; a failure or
-- a crash between the two runs the row again. The claim deduplicated only against a delivery still
-- pending, so a retry after the architect's turn on the task had ended and retired it queued the
-- whole task again, and the architect was told each decision in it twice. The claim records the
-- id of every start row whose task it queued, in the transaction that writes that delivery, so a
-- retry of that row queues nothing whether its delivery is pending or retired. The rows of one
-- issue do not run in row order, so the set is kept rather than a high-water mark: an older start
-- that has never reached this claim still gives its task.
--
-- It is also what says whether a start's task reached the claim, which is what settles the
-- decisions that start named (0065_parked_decisions): a start the root architect's claim queued
-- forgets what it named, and one it did not hands those claims over to a fresh catch-up, so a
-- release never leaves a kept decision unclaimed while its root has an architect to tell.
alter table claims add column delivered_rows bigint[] not null default '{}';

-- pending_task_deliveries.kept_decisions is what that task names of the decisions a person made
-- while nobody in the root's tree could hear them (supervise.Delivery.Kept, record.KeptDecisions as
-- JSON), kept with the delivery because the rows themselves are forgotten the moment the claim
-- queues the task: from then on the task is the only thing that carries them, and it can wait on a
-- ready or idle claim for generations while its root is parked, lingering or waiting for its slot
-- (supervise's Deps.RootHears). One reader: the hand-over of a child into a tree of its own, which
-- re-keeps for the child, unclaimed under the child's key, every decision on the child's subtree
-- that such a held, unconfirmed task names, so the child's own architect is told it
-- (record.Store.ReKeepDeliveredDecisions, admit.Admission.readmit). An empty object is a task that
-- names none.
alter table pending_task_deliveries add column kept_decisions jsonb not null default '{}'::jsonb;

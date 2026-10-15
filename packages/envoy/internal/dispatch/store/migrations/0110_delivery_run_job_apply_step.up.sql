-- LEGION-761: a production job counts as a deploy only when it applied. Intake and the reconcile
-- read each job's steps from GitHub's jobs listing; for a job whose name delivery_settings lists,
-- while production_apply_step_name is set, they record the step name it was read for and that step's
-- conclusion (delivery.RunJobs). Both stay null on every other job and on a row stored before this
-- column, which keeps its own conclusion (delivery.Production.Conclusion). No check constraint: one
-- would hold this table ACCESS EXCLUSIVE while it validates every row, and Go writes only
-- mapJobConclusion's values. No row is rewritten here.
alter table delivery_run_jobs
  add column apply_step text,
  add column apply_step_conclusion text;

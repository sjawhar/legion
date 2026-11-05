-- 20261102154500_pod_hold_streaks.up.sql — a pod's hold streak ends (INFRA-945).
--
-- pod_holds is one row per streak of the idle rule keeping a pod for one reason, and a streak has
-- to be able to end: keyed by (project, issue, reason) for all time, a pod held for `image` on
-- Monday and again on Friday was one row carrying Monday's first_at and both days' passes, which
-- is not a streak and reads wrongly in every window between them.
--
-- ended marks a streak that is over, and the unique index that the upsert keys on now covers the
-- running streak alone, so the next pass of a reason whose streak ended starts a streak of its
-- own with its own first_at. A streak ends when the pod is released (RecordPodRelease), when the
-- rule reads another reason for that pod, and when a pass of the rule holds it for no reason
-- worth a row (store.EndPodHoldStreaks, through the daemon's recordIdleHold).
--
-- The shipped rows are the streaks that were running when this migration ran: they keep their
-- first_at and their passes and stay the running streak of their reason, since nothing about
-- them is known to have ended.
alter table pod_holds add column ended boolean not null default false;
drop index pod_holds_project_issue_reason;
create unique index pod_holds_running_project_issue_reason on pod_holds (project, issue, reason) where not ended;

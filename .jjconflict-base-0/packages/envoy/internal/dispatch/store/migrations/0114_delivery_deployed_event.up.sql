-- LEGION-781: the once-per-pull-request marker of the issue.deployed event the delivery timeline
-- wrote on the pull request's issue (delivery.RecordDeployedEvents). Null until the timeline first
-- knows the production run that shipped the pull request, by intake or by the reconcile; set in the
-- transaction that appends the event, so a second pass, or the two running at once, never writes it
-- twice. Nullable and unindexed: the rows it selects are the deploy repository's merged pull
-- requests with an issue and no marker, a few thousand at most, read by repo.
alter table delivery_pull_requests add column deployed_event_at timestamptz;

-- A pull request whose shipping run is already known when this column arrives shipped before the
-- event existed, so it is stamped here and the first pass writes no event for it: the event tells
-- the Legion daemon a check's merge reached production, and no check waits on one of these. The
-- rule is the timeline's own (delivery.shippingRunJoin, which ListPullRequestsWaitingAt and the
-- deployed pass share): the earliest-completing successful production run of the deploy
-- repository's main branch created after the merge, its production job one the settings name and,
-- when the settings name an apply step, that step successful or absent. A merged pull request with
-- no such run yet keeps null and gets its event when the run arrives, which an issue merged just
-- before the deploy, and closed by the daemon's first boot on this release, waits for.
update delivery_pull_requests pr
set deployed_event_at = now()
from delivery_settings s
where pr.repo = s.deploy_repo
  and pr.merged_at is not null
  and pr.deployed_event_at is null
  and exists (
    select 1
    from delivery_runs r
    join delivery_run_jobs j on j.repo = r.repo and j.run_id = r.run_id and j.name = any(s.production_job_names)
    where r.repo = pr.repo and r.kind = 'deploy' and r.head_branch = 'main' and j.conclusion = 'success'
      and (s.production_apply_step_name is null or j.apply_step is distinct from s.production_apply_step_name or j.apply_step_conclusion = 'success')
      and j.completed_at is not null and coalesce(r.created_at, r.started_at) > pr.merged_at);

-- Per-step reconcile progress: how far back in time each reconcile step (delivery/reconcile.go)
-- has actually imported, so a pass that fails part way resumes where it stopped instead of
-- restarting its whole window on the next pass.
--
-- delivery_settings.last_reconcile_at stays what it was -- "the last pass in which every step
-- succeeded" -- but no step reads it any more: a 28-day backfill of a busy repository is tens of
-- thousands of GitHub requests, far past one installation's hourly rate limit, so a single pass
-- cannot finish one and an all-or-nothing timestamp meant every pass restarted from 28 days ago
-- and none ever completed.
--
-- step is the step's own name ('runs/deploy', 'runs/pr_checks',
-- 'merged_pull_requests/installation/<id>'), and scope what that step's progress was measured
-- against (the repository and workflow path for a run listing, the population and exclusions for
-- a search): a settings change that moves the scope leaves the row in place but makes it not
-- match, so the step backfills afresh rather than resuming a window measured against settings
-- that no longer apply. through is the end of the last window the step imported completely.
--
-- A new table: no index or trigger here touches an existing one, so nothing is locked.
create table delivery_reconcile_progress (
    step text primary key,
    scope text not null,
    through timestamptz not null,
    updated_at timestamptz not null default now()
);

-- LEGION-730: GitHub's created_at on every stored run. A pull request ships with the first
-- successful production deploy whose run GitHub created after its merge (delivery/containment.go):
-- the head commit's time cannot decide it, since a repository that merges through GitHub's merge
-- queue writes the merge commit before GitHub records the merge, and started_at cannot either,
-- since a re-run moves it to the re-run's start. Null on a row stored before this column existed,
-- where readers take started_at (DeliveryRun.Created, store.go's runCreatedAt): the resumable
-- backfill (delivery.Reconcile.backfillRuns, step 'backfill/created_at/deploy') re-lists the last
-- 28 days of deploy runs once, filling it; no row is rewritten here.
alter table delivery_runs add column created_at timestamptz;
-- ListRuns and ListPullRequestsWaitingAt filter and order deploy runs by their creation time, read
-- through the same coalesce. ListRuns was the one reader of delivery_runs_kind_started, on
-- head_commit_at, which no query filters or orders on now.
create index delivery_runs_kind_created on delivery_runs (repo, kind, (coalesce(created_at, started_at)));
drop index delivery_runs_kind_started;

-- LEGION-781: the once-per-pull-request marker of the issue.deployed event the delivery timeline
-- wrote on the pull request's issue (delivery.RecordDeployedEvents). Null until the timeline first
-- knows the production run that shipped the pull request, by intake or by the reconcile; set in the
-- transaction that appends the event, so a second pass, or the two running at once, never writes it
-- twice. Nullable and unindexed: the rows it selects are the deploy repository's merged pull
-- requests with an issue and no marker, a few thousand at most, read by repo. No row is rewritten
-- here: a pull request already shipped before this column gets its event on the first pass after it.
alter table delivery_pull_requests add column deployed_event_at timestamptz;

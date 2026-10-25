-- 0116_artifact_reviews_event_seq.up.sql
-- The sequence of the artifact.approved or artifact.changes_requested event each review appended on
-- its document's owner (its issue, else the document), which docs.WriteReview stores in the
-- transaction that appends the event. The owner's row lock orders those events as they commit, so
-- the sequence places a review among its issue's other events where created_at, the transaction's
-- start, cannot: a review whose transaction began before another event's and committed after it has
-- the later sequence and the earlier time. The approval read returns it as `seq`, which the Legion
-- daemon orders a review against the start of a root's wait by. Null for a review recorded before
-- this column, and nullable, so the release before it keeps inserting reviews without it.
alter table artifact_reviews add column event_seq bigint check (event_seq > 0);

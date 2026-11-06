-- 20261102110200_artifact_review_kinds.up.sql
--
-- Who gave a review. Every review stored until now is a person's, given by answering an approval
-- request or from the document header, or carried to a later version in a person's name; a Plan
-- document is approved instead by the plan review an agent runs, which no human is ever asked to
-- approve. The column says which:
--   kind  'person' for a review a person gave or Dispatch carried in their name, 'plan_review'
--         for the approval a plan review records of a plan version (POST
--         /api/v1/artifacts/{id}/plan-reviews, refused on any document whose role is not 'plan').
-- A plan review's state is 'approved', so the row's event is artifact.approved and migration
-- 0120's trigger, which reads the state alone, gives it its sequence as it gives every other
-- review one. The derived approval of a plan version goes stale at a later version exactly as a
-- spec's does: the reads compare the latest review's version with the document's.
--
-- The column is not null with the constant default 'person', which rewrites no row (Postgres
-- stores such a default in the catalog), and its check admits that default, so the statement
-- holds artifact_reviews' ACCESS EXCLUSIVE lock for the catalog change alone. The release before
-- this one inserts reviews without naming the column and gets 'person', the only kind it writes,
-- so it rolls beside this one (cmd/dispatch/README.md, "Database migrations"). The census
-- answers 0.
alter table artifact_reviews
    add column kind text not null default 'person'
        constraint artifact_reviews_kind_check check (kind in ('person', 'plan_review'));

-- 0122_reference_excerpt_backoff.up.sql
-- One row per document the reference excerpt reconciliation
-- (docs.Service.RunReferenceExcerptReconciliation) could not read or record: the version it failed
-- at, how many times in a row it failed there, and when it tries again (refs.DeferExcerpts: one
-- minute after the first failure, doubling, at most six hours, plus up to a fifth of jitter).
-- refs.DriftedExcerpts leaves a document out of a batch until its row falls due, so a document
-- that keeps failing no longer fills every batch and holds back the documents that sort after it,
-- and the earliest next_attempt_at is the reconciliation's one timer while nothing is written. A
-- row naming an older version than the document's latest holds nothing back; a success deletes
-- the row. The table holds only failing documents, so it has no index but its key. The previous
-- release never reads it.
create table reference_excerpt_backoff (
  artifact_id uuid primary key,
  version integer not null,
  attempts integer not null,
  next_attempt_at timestamptz not null
);

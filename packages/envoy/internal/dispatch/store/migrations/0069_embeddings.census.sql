-- 0069 creates new tables (embeddings, embeddings_backfill_progress), the vector extension, and
-- triggers on issues/artifact_versions/comments/asks/messages that only enqueue future embedding
-- work (an insert, never a rewrite, of a row keyed by (kind, id) that cannot yet exist). It
-- refuses and rewrites no existing row.
select 0;

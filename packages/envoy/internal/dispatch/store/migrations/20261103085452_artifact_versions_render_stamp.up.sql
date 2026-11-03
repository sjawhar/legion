-- 20261103085452_artifact_versions_render_stamp.up.sql — what a version's markdown was rendered
-- from, so a read serves it instead of rendering the document again.
--
-- sjawhar/legion#2712 (INFRA-635): a version already stores the markdown of the tree its writer
-- wrote, and a text read of the document renders that same tree over again (docs' TextWithToken).
-- These four columns say which state the stored markdown is the rendering of, so a read that finds
-- the document still standing in that state serves the stored text:
--
-- - head_version is the head the markdown was rendered at, the version a load folds the stored
--   updates through (docs' headVersion: a prune's checkpoint ceiling while one stands, else the
--   greatest stored version, 0 for none). It is not doc_update_version, which holds the writer's
--   own cursor and disagrees with the head while a prune checkpoint stands; the reader compares
--   the head, so the head is what is stored.
-- - doc_update_row is the transaction that wrote the doc_updates row standing at head_version,
--   Postgres's xmin as text, null where no row stands there. The version alone does not name a
--   state - a prune deletes the rows past its ceiling and the next append takes the number the
--   first of them had - while the row's transaction does, and a compaction that rewrites the head
--   row gives it a new one (docs' DocumentStamp).
-- - document_token is the document token of that tree (docs' nodeToken), which the read answers
--   beside the markdown.
-- - renderer_version is the renderer version of the build that wrote the row
--   (pmdoc.RendererVersion); a stored rendering is served only while it stands at the number this
--   build holds.
--
-- All four are nullable with no default, so the add rewrites no row: every version already stored,
-- and every version a writer leaves unstamped (a tree with no token, an upload whose replacement
-- changed nothing), reads with the stamp null, and a read that finds a null doc_update_row renders
-- the document as it did before this migration. The release before this one writes none of them
-- and reads none of them, so the column add is all there is to roll back. The statement holds
-- artifact_versions in ACCESS EXCLUSIVE for the catalogue write alone, no row being read or
-- rewritten.
alter table artifact_versions
  add column head_version bigint,
  add column doc_update_row text,
  add column document_token text,
  add column renderer_version integer;

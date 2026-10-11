-- 0092_refs_excerpts.up.sql
-- Stores on each mention a document writes the excerpt GET /api/v1/references answers it with:
-- the id of the block holding the target's first mention (the dashboard's #b-<id> jump) and that
-- block's markdown cut to 480 characters, as of document version excerpt_version. Every version
-- write sets them on every mention of its document in its own transaction (refs.RecordExcerpts),
-- or, when it cannot cut them, clears excerpt_version alone, so a mention whose excerpt_version is
-- null is one the server's reconciliation (docs.Service.RunReferenceExcerptReconciliation) still
-- owes an excerpt: every row this migration finds, each one rebuild-refs inserts, and those. The
-- route never renders a document to cut one (LEGION-531). The three columns are nullable with no
-- default, a catalog change that rewrites no row. refs_excerpt_drift holds exactly the rows the
-- reconciliation owes, so once it has caught up each of its ticks reads an empty index rather than
-- every document's mentions.
alter table refs
  add column excerpt_block_id text,
  add column excerpt_text text,
  add column excerpt_version integer;
create index refs_excerpt_drift on refs (from_id) where from_kind = 'artifact' and excerpt_version is null;

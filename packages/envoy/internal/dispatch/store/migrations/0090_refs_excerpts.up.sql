-- 0090_refs_excerpts.up.sql
-- Stores on each mention a document writes the excerpt GET /api/v1/references answers it with:
-- the id of the block holding the target's first mention (the dashboard's #b-<id> jump) and that
-- block's markdown cut to 480 characters, as of document version excerpt_version. Every version
-- write fills them in its own transaction (refs.RecordExcerpts), and the server's reconciliation
-- (docs.Service.RunReferenceExcerptReconciliation) fills every row whose excerpt_version is behind
-- its document's latest version - every row this migration finds among them - so the route never
-- renders a document to cut one (LEGION-531). Three nullable columns with no default: a catalog
-- change that rewrites no row.
alter table refs
  add column excerpt_block_id text,
  add column excerpt_text text,
  add column excerpt_version integer;

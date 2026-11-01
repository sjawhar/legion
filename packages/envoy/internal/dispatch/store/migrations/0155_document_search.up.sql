-- 0155_document_search.up.sql
-- One row for each document, holding the search vector of its latest version, with a GIN index
-- over that vector. Search's document arm tests every document's latest version through a lateral
-- join over artifacts (api/search.go searchKeywordLegs), because artifact_versions.search has no
-- index and can have none: 0010 left it out, since an index over every version would also find a
-- word that only an old version holds, and artifact_versions is 5.2 GiB, above the size the
-- pre-deploy census lets a migration touch. A table holding exactly the versions search reads is
-- an index over them that the census allows.
--
-- Four columns, and no more. artifact_id is the key one row per document needs; number is what
-- the write guard of 0156's trigger reads, so a late write of an older version never replaces the
-- latest; created_at is the other column the lateral supplies today (coalesce(i.updated_at,
-- v.created_at) in the keyword arm, and the meaning candidate CTE reads nothing else from the
-- version), so without it the lateral would stay; search is the vector the index is on. A
-- document's project, kind and issue come from a join to artifacts, as they do today.
--
-- No foreign key, to artifacts or to artifact_versions: a `references` makes the pre-deploy
-- census size the table it points at (internal/pgmigrate/touched.go), and artifact_versions is
-- above the limit, so a key to it would refuse every deploy carrying this migration.
--
-- No delete path, and no trigger for one. Every reader of this table joins artifacts on
-- a.kind = 'doc', so a row for a document that no longer exists cannot surface, and no production
-- code path deletes an artifact or an artifact_version - the only deletes of either in the tree
-- are in tests. STRATEGY.md says to build no protection against a failure nobody has seen, so the
-- release that first deletes a document is the release that adds the delete path.
create table document_search (
  artifact_id uuid primary key,
  number integer not null,
  created_at timestamptz not null,
  search tsvector not null
);
create index document_search_vector on document_search using gin (search);

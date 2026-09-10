-- 0010_search.up.sql
-- Full-text search columns. Generated columns: Postgres computes them on every write;
-- no application code refreshes them. Issue keys and titles carry weight A so a title
-- hit outranks a body hit. artifact_versions gets the stored vector without an index:
-- the search reads exactly one latest row per document through
-- artifact_versions_artifact_id_number_key, and an index over every version could only
-- surface stale versions.
alter table issues add column search tsvector generated always as
  (setweight(to_tsvector('english', key), 'A') || setweight(to_tsvector('english', title), 'A')) stored;
create index issues_search on issues using gin (search);
alter table artifact_versions add column search tsvector generated always as
  (to_tsvector('english', coalesce(markdown, ''))) stored;
alter table comments add column search tsvector generated always as
  (to_tsvector('english', body)) stored;
create index comments_search on comments using gin (search);
alter table asks add column search tsvector generated always as
  (to_tsvector('english', question || ' ' || coalesce(answer->>'text', ''))) stored;
create index asks_search on asks using gin (search);
alter table messages add column search tsvector generated always as
  (to_tsvector('english', body)) stored;
create index messages_search on messages using gin (search);

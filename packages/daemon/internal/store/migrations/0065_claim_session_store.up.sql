-- 0065_claim_session_store.up.sql — a claim records the store that held its session so a later
-- runtime can refuse to reap the old volume without reading the session database. Every existing
-- claim predates database session tracking, so its session is conservatively a file-store session.
alter table claims add column session_store text not null default 'pvc'
  check (session_store in ('pvc', 'postgres', 'cloudsql-iam'));

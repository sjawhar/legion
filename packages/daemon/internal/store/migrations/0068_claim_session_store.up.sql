-- 0068_claim_session_store.up.sql — a claim records the store that held its session so a later
-- runtime resolves the unknown value from its configured store on its first boot.
alter table claims add column session_store text not null default ''
  check (session_store in ('', 'pvc', 'postgres', 'cloudsql-iam'));

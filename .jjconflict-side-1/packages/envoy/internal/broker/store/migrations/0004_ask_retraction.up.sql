-- When the broker closed the Dispatch ask of a request that ended without an answer (cancelled or
-- expired), so no human is left answering a question whose answer changes nothing; null while the
-- ask may still be open. Additive and nullable, safe on a fresh or already-migrated database.
alter table requests add column if not exists ask_retracted_at timestamptz;
alter table launcher_credential_requests add column if not exists ask_retracted_at timestamptz;

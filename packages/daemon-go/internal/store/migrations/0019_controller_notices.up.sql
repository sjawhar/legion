-- 0019_controller_notices.up.sql — the controller notices no session held the controller role to
-- take. The outbox keeps one here, under the dedupe key its row published it with, in the transaction
-- that finishes the row, so a refused notice is neither dropped nor retried; the daemon publishes the
-- kept ones to the controller role, oldest first, once a controller holds it, and deletes each the
-- listener takes. A notice is its issue's project's, as an outbox row is.
create table controller_notices (
  id bigserial primary key,
  issue text not null,
  dedupe_key text not null unique,
  notice jsonb not null,
  created_at timestamptz not null default now());

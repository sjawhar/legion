-- INFRA-258: one row per write a caller named with an Idempotency-Key header, which every POST and
-- PATCH route takes (api/request_keys.go). The dashboard and the agents' client mint one key per
-- intent and retry under it after a failed fetch, a timeout, a 5xx or a 429, none of which says
-- whether the first attempt ran; this row is what makes that retry safe. A repeat of the key with
-- the same request is answered the first request's stored status and body and runs nothing again;
-- the key with another request is refused 422 KEY_REUSED; a repeat while the first still runs is
-- 409 KEY_IN_FLIGHT. It replaces the two hand-written key tables: broadcast_idempotency_keys
-- (migration 0055), which this release neither reads nor writes and a later release drops, and the
-- single-session send's table, which this branch added and never shipped.
--
-- login is whom the key belongs to: a person's canonical login (model.CanonicalLogin), whether they
-- write from the dashboard or through an agent's personal token, a service token's subject behind
-- `service:`, or `agent-token` for the shared agent token. request_digest is a SHA-256 of the method,
-- the route pattern, its path values, the query, the media type and the body (a multipart body with
-- its boundary set aside, which a client mints afresh for every attempt), so a key reused for
-- another request is told from a repeat. method and path are the first request's, which a 422
-- names.
--
-- A row is in flight while status is null. attempt and leased_at are its lease: the attempt that
-- holds the key, and when it took it. A repeat past the lease (IdempotencyLease, a minute) presumes
-- the holder's process died and takes the key over, attempt + 1; every write the holder makes to
-- this row afterwards is scoped to its own attempt, so the one it no longer holds changes nothing.
-- A null leased_at is a lease released at once (a 5xx after the request committed its recovery
-- point). status, content_type and response are the stored answer: response null with a status
-- set is an answer larger than Dispatch keeps (1 MiB), replayed as ANSWER_NOT_KEPT. A 5xx, 401,
-- 408 and 429 are never stored.
--
-- recovery is the recovery point a write that also mutates foreign state commits with its own rows,
-- in the same transaction (Brandur Leach's "Implementing Stripe-like Idempotency Keys in
-- Postgres"): the single-session send's message and the broadcast it made. A row that holds one
-- is answered from it - the delivery still owed resumed, then what it made read again - however
-- its first request ended, so a send is never made twice even when the process died between its
-- commit and this row's answer.
--
-- created_at bounds a key's life: the suggestion-outcome sweep's pass deletes every row older than
-- IdempotencyKeyTTL (24 hours), on the created_at index, and a repeat that finds an older row not
-- yet swept treats the key as unused.
--
-- A new table: nothing existing is read, locked or rewritten here, and the release before this one
-- neither reads nor writes it, which is what a rolling update requires of a migration
-- (cmd/dispatch/README.md, "Database migrations").
create table request_keys (
  login text not null,
  idempotency_key text not null,
  request_digest text not null,
  method text not null,
  path text not null,
  attempt integer not null default 1,
  leased_at timestamptz default now(),
  status integer,
  content_type text,
  response bytea,
  recovery jsonb,
  created_at timestamptz not null default now(),
  primary key (login, idempotency_key)
);

create index request_keys_created_at on request_keys (created_at);

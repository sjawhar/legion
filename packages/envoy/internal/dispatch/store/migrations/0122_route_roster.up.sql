-- 0122_route_roster.up.sql
-- INFRA-562: the listener's last answer to GET /v1/sessions, which every issue read judges its
-- route_status against (api/issue_route_status.go). Each process used to keep its own copy, so two
-- processes asked the listener moments apart and could read one route live on one and no_holder on
-- the other, and a filtered page cut on one held other issues than the next page cut on the other.
-- One stored answer serves every process: a read judges against this row while it is fresh, and a
-- process that finds it stale or missing asks the listener once (its own single-flight fetch) and
-- stores the answer here, where the other process reads it.
--
-- One singleton row (id enforced true by the check, so a second insert never succeeds), as
-- embeddings_rate_limit (0078) is:
--   answered    false when the listener failed or did not answer: every route then reads unknown
--               on every process until expires_at, rather than each process waiting out the
--               listener's silence of its own.
--   sessions    the live sessions as the listener listed them, each only {session_id, roles,
--               last_seen}, which is all a route's reach is judged by; '[]' when not answered.
--   sent_at     when the call that got this answer was sent, by the database's clock. An answer
--               is stored only over an older one (`where route_roster.sent_at < excluded.sent_at`),
--               so a slow call that answers after a later one never replaces the newer answer.
--   expires_at  until when the row serves reads, judged against the database's now(): sent_at
--               plus the answer's TTL, or, for a failure, the moment it was known plus the silence.
--               The database's clock is the one both processes read, so they agree on freshness
--               whatever their hosts' clocks say.
--
-- A new table: nothing existing is read, locked or rewritten, and the release before this one
-- neither reads nor writes it, so it rolls beside this one (cmd/dispatch/README.md, "Database
-- migrations"), keeping its per-process copy while this release reads the shared row.
create table route_roster (
    id boolean primary key default true check (id),
    answered boolean not null,
    sessions jsonb not null,
    sent_at timestamptz not null,
    expires_at timestamptz not null
);

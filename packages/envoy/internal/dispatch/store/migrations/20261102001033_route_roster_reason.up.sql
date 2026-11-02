-- 20261102001033_route_roster_reason.up.sql
--
-- Why the listener gave no session list, on the one row every process reads
-- (api/issue_route_status.go). route_roster (0159) recorded only that the listener answered or
-- did not, so the process whose call met the listener's refilling answer read "refilling" while
-- every other process read the same row as "unreachable": one issue page said the listener is
-- refilling and the next said it is unreachable, and GET /api/v1/agents answered
-- 503 ENVOY_REFILLING on one process and 503 ENVOY_UNAVAILABLE on the other, which
-- dispatch://INFRA-261's acceptance forbids.
--
--   reason          why the listener gave no list, as the fetching process saw it:
--                   model.RouteReasonRefilling or model.RouteReasonUnreachable. '' for an
--                   answered list, which needs none.
--   retry_after     the Retry-After the listener sent with its refilling answer, as it sent it;
--                   '' when it sent none and for any other reason.
--   reason_sent_at  the sent_at the two above belong to; null for a row written with neither.
--
-- A read takes the reason only while reason_sent_at is the row's own sent_at, because the release
-- before this one updates the row without naming these three columns: its upsert moves sent_at
-- and leaves the reason of the answer before it, and a listener that is down would then read as
-- refilling for as long as that answer serves. Under the test, a silence that release stores
-- carries no reason of its own and reads unreachable, which is exactly how that release answers
-- it, so the two agree on every silence either can tell apart.
--
-- Three columns added to a table of at most one row, two with a constant default and one
-- nullable: a catalog change that rewrites no row, under route_roster's ACCESS EXCLUSIVE lock for
-- milliseconds. The release before this one keeps reading and writing the row, naming its own
-- columns alone (cmd/dispatch/README.md, "Database migrations"), so this rolls beside it. The
-- census answers 0, and rolling back past it is an image rollback alone.
alter table route_roster
    add column reason text not null default '',
    add column retry_after text not null default '',
    add column reason_sent_at timestamptz;

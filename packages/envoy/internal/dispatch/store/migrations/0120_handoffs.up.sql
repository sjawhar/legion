-- 0120_handoffs.up.sql — phase handoffs recorded on their issue.
--
-- INFRA-177: Legion's phase handoffs move from committed `.legion/<KEY>/<phase>.json` files to
-- records on the Dispatch issue. One row per handoff a session writes
-- (POST /api/v1/issues/{key}/handoffs/{phase}, api/handoffs.go):
--
-- - phase is a lowercase slug, the route's `{phase}` (Legion writes plan, implement, test and
--   review); the check holds the rule the API refuses first, so direct SQL cannot write another.
-- - version counts from 1 within the issue and phase. The API writes coalesce(max(version), 0) + 1
--   in the insert itself, after requireOpenIssue has locked the issue row for no key update, so two
--   writes to one issue take their versions one after the other; the unique constraint holds that
--   no two rows ever share one.
-- - body is the exact text the session sent, one JSON object the API checks on write, in a text
--   column rather than jsonb so a read answers the bytes written: jsonb would reorder keys, drop
--   duplicates and whitespace, and refuse the \u0000 escape. The API caps it at
--   contracts.DispatchHandoffBodyMax bytes.
-- - author is the writing session's actor, as messages.author is.
--
-- The rows go with their issue (on delete cascade). A new table: nothing existing is read or
-- rewritten, and the release before this one never names it, so it rolls (cmd/dispatch/README.md,
-- "Database migrations"). Its foreign key holds issues in SHARE ROW EXCLUSIVE only while the table
-- is created.
create table handoffs (
  id uuid primary key default gen_random_uuid(),
  issue_key text not null references issues(key) on delete cascade,
  phase text not null check (phase ~ '^[a-z][a-z0-9-]{0,31}$'),
  version integer not null check (version > 0),
  body text not null,
  author jsonb not null,
  created_at timestamptz not null default now(),
  unique (issue_key, phase, version)
);

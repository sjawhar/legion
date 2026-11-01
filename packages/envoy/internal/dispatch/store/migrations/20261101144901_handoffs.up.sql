-- 20261101144901_handoffs.up.sql — phase handoffs recorded on their issue.
--
-- sjawhar/legion#2354: Legion's phase handoffs move from committed `.legion/<KEY>/<phase>.json`
-- files to records on the Dispatch issue. This migration creates handoffs, the phase handoffs a
-- session records on its issue (packages/envoy/AGENTS.md): one row per handoff a session writes
-- (POST /api/v1/issues/{key}/handoffs/{phase}, api/handoffs.go), holding id, issue_key, phase,
-- version, body, author and created_at.
--
-- - id is the row's own uuid, the record GET /api/v1/handoffs/{id} answers.
-- - issue_key is a foreign key to issues, on delete cascade.
-- - phase is checked to be a lowercase slug, the route's `{phase}` (Legion writes plan, implement,
--   test and review); the check holds the rule the API refuses first, so direct SQL cannot write
--   another.
-- - version counts from 1 within the issue and phase. The API writes coalesce(max(version), 0) + 1
--   in the insert itself, after requireOpenIssue has locked the issue row for no key update, so two
--   writes to one issue take their versions one after the other; the unique constraint holds that
--   no two rows ever share one.
-- - body is the exact text the session sent, one JSON object the API checks on write, in a text
--   column rather than jsonb so a read answers the bytes written: jsonb would reorder keys, drop
--   duplicates and whitespace, and refuse the \u0000 escape. The API caps it at
--   contracts.DispatchHandoffBodyMax bytes.
-- - author is the writing session's actor, as messages.author is.
-- - created_at is when the row was written.
--
-- The rows go with their issue (on delete cascade). A new table that no earlier release names:
-- nothing existing is read or rewritten, and the release before this one never names it, so it
-- rolls (cmd/dispatch/README.md, "Database migrations") and its census answers 0. Its foreign key
-- holds issues in SHARE ROW EXCLUSIVE only while the table is created. Its identifier
-- is the UTC moment it was written, as every migration after 0149_event_horizon's is.
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

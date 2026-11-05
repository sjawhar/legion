-- 20261101214123_claim_session_activity.up.sql — what a live role's session last reported of what
-- it is doing, so the idle rule can decide whether its pod may be released. The session sends the
-- report after each of its turns and whenever its content changes (POST /legion/v1/claims/activity),
-- and the route writes these four columns and nothing else of the row: a machine's own claim write
-- neither carries them nor erases them, so a report that arrived since the machine read its claim
-- survives the next state it persists.
--
-- activity_at is when the session read the report, null for a claim that has reported none; a
-- claim with no report after its last turn is never released, which is what makes a host that
-- cannot report safe rather than lost. activity_background is whether that reading found
-- background work of the session's own running and activity_kinds the kinds it named
-- (async-job, subagent, claude-background-task), comma-joined in the sorted, de-duplicated order
-- the session sent them, so the reason a hold records reads the same every time.
-- activity_next_reminder_at is when the phase reminder armed in that session is due, null when no
-- armed reminder was live. No backfill: every live claim reports again at its next turn.
alter table claims add column activity_at timestamptz;
alter table claims add column activity_background boolean not null default false;
alter table claims add column activity_kinds text not null default '';
alter table claims add column activity_next_reminder_at timestamptz;

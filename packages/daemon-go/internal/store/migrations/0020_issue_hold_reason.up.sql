-- 0020_issue_hold_reason.up.sql — why a held issue is held, when a hold has a reason.
--
-- The architect's escalation of a held issue is recorded here as `escalated`, so the controller,
-- which learns of holds from the issue record at every start, can tell an escalation waiting on
-- it from a hold its architect is still deciding. The store writes it only while the issue is
-- held, so a hold's end clears it.
alter table issues add column hold_reason text;

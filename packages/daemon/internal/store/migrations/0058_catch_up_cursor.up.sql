-- 0058_catch_up_cursor.up.sql -- where the boot catch-up of each root last read Dispatch's
-- event log to.
--
-- caught_up_seq is the issue's Dispatch event sequence the daemon's boot catch-up last applied the
-- issue's event log through (the daemon's catch-up of the roots at admitted or awaiting_person).
-- A boot reads only the events after it, applies them through the live handlers with the root held
-- out of admission's decision, then decides the root once and raises the cursor to the sequence it
-- read through. It is never lowered.
--
-- An issue recorded before this migration has no cursor (null): its first catch-up after the
-- deploy replays nothing, and sets the cursor to the issue's latest sequence on Dispatch, since
-- every event before it reached a daemon of an earlier release live, and that release's record is
-- what this release decides from. An issue recorded after it starts at 0, which the catch-up reads
-- as no cursor yet: it reads from the root's last handout (issues.handout_seq), as the record
-- already holds everything before it.
alter table issues add column caught_up_seq bigint check (caught_up_seq >= 0);
alter table issues alter column caught_up_seq set default 0;

-- 0154_write_suggestions_swept.up.sql
-- Which suggestions the outcome sweep (api.RunSuggestionOutcomeSweep) has yet to judge against
-- every event of theirs. A pass reads the event log after its cursor (suggestion_outcome_cursor,
-- migration 0153), but a suggestion is recorded after its write commits (api.persistSuggestions),
-- so it can commit after an event that started later than it and that a pass has already read.
-- write_suggestions.id is a uuid, and no id or timestamp of such a row commits in order, so no
-- cursor can find such a row. swept is false until a pass has judged the row against all its
-- events, and the pass sets it for the rows it judged; a row the previous release inserts during
-- a deploy takes the default and is judged too. write_suggestions_unswept serves the pass's read
-- of those rows, oldest first.
--
-- Existing rows take true, through the first default, so the column is added without rewriting
-- the table and write_suggestions_unswept starts empty: the first pass on this schema judges every
-- pending suggestion anyway (suggestion_outcome_cursor.event_id null). New rows take false.
--
-- write_suggestions_pending_artifact serves the pass's lookup of pending suggestions by a new
-- event's document (source_artifact_id), the one column of that lookup that had no index: the
-- previous release only ever probed events from each pending row, never the reverse.
alter table write_suggestions add column swept boolean not null default true;
alter table write_suggestions alter column swept set default false;
create index write_suggestions_unswept on write_suggestions (created_at) where not swept;
create index write_suggestions_pending_artifact on write_suggestions (source_artifact_id)
  where outcome = 'ignored' and source_artifact_id is not null;

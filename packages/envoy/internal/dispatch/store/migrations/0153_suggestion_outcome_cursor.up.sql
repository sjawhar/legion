-- 0153_suggestion_outcome_cursor.up.sql
-- How far the suggestion outcome sweep (api.RunSuggestionOutcomeSweep) has read the event log: one
-- row, whose event_id is the highest events.id a pass has judged the pending write_suggestions
-- against. A pass reads only the events after it, and judges only the pending suggestions those
-- events' issues and documents name plus the suggestions recorded since the last pass
-- (write_suggestions.swept, migration 0154), so its work follows what changed rather than how
-- many suggestions are still pending. The pass holds this row `for update` in its transaction,
-- which is also what keeps two passes from judging one page twice.
--
-- event_id starts null: the first pass on a database runs the three rules over every pending
-- suggestion once, as every pass of the previous release did, and records the highest event id it
-- read before them. A pass reads only up to the event log's safe horizon (events.Horizon) and
-- never moves event_id past it: event ids are not commit order, so an uncapped `id > event_id`
-- read could pass an event still committing. A new table; the previous release never reads it.
create table suggestion_outcome_cursor (
  singleton boolean primary key default true check (singleton),
  event_id bigint
);
insert into suggestion_outcome_cursor default values;

-- 0025_phase_completed_at.up.sql — when the workflow applied a role's completion of its current
-- phase (record.PhaseRow.CompletedAt).
--
-- A reviewer's completion arrives through the API and its review by webhook, in either order, so a
-- review it submitted before completing can be delivered after. The round orders its reviews against
-- the completion by this time: only a review submitted after it is the reviewer's answer to a round
-- it left undecided. A row written before this column holds no time, as a phase not yet completed.
alter table phases add column completed_at timestamptz not null default '0001-01-01 00:00:00+00';

-- 0026_phase_completed_at.up.sql — when the workflow applied a role's completion of its current
-- phase (record.PhaseRow.CompletedAt).
--
-- A reviewer's completion arrives through the API and its review by webhook, in either order, so a
-- review it submitted before completing can be delivered after. The round takes a review as the
-- reviewer's answer to a round it left undecided only when it was submitted more than a bounded
-- clock skew after this time (workflow's reviewersAnswer and answerSkew). A row written before this
-- column holds the zero time, which reads back as no time.
alter table phases add column completed_at timestamptz not null default '0001-01-01 00:00:00+00';

-- 0005_phase_last_handoff.up.sql — the carrying commit each role last reported for a file-backed
-- phase, kept across the next phase's start so a completion that reports it again is refused.
alter table phases add column last_handoff text not null default '';

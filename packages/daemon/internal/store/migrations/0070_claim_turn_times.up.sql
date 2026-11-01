-- 0070_claim_turn_times.up.sql — the last turn a claim's agent started and the last one it ended,
-- the pair a reader needs to tell a worker that is working from one that has stopped making
-- progress.
--
-- The machine already sees both events: a turn's start moves the claim to working and its end
-- moves it back to idle (supervise/table.go). Neither was kept, so the times lived only in the
-- stream that carried them, a restart lost them, and an operator reading a claim could not tell
-- one idle since a minute ago from one idle since yesterday, nor a working claim whose turn began
-- moments ago from one wedged in a turn for hours.
--
-- Every existing row takes null for both: no claim that ran before this release recorded a turn,
-- and a backfill would have to invent a time it does not know. Null is how a reader tells "no
-- turn is recorded" from a turn that happened, which a zero timestamp reading as 1970 would hide.
alter table claims add column last_turn_start timestamptz;
alter table claims add column last_turn_end timestamptz;

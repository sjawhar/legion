-- 20261107052815_claim_step_record.up.sql — what the daemon last saw a worker's agent do, so a
-- turn that hangs is found rather than read as hours of work (INFRA-1274). Oh My Pi writes a
-- tool_execution_start and a tool_execution_end frame for every call the agent makes, and the
-- daemon keeps the last of them on the claim; a backstop over these columns is what ends a turn
-- whose agent has stopped stepping. Store.PutClaimStep writes these eight columns and nothing
-- else of the row: a machine's own claim write neither carries them nor erases them, so a step
-- recorded since the machine read its claim survives the next state that machine persists.
--
-- step_at is when the daemon received the claim's last step, null for a claim that has stepped
-- none. The daemon stamps it on receipt because no frame carries a time of its own, and a frame
-- replayed from a dead connection's backlog does not move it: that frame is evidence of what the
-- agent was doing and no evidence at all that it is doing it now.
--
-- turn_started_at is when the agent's current turn began. It is what a claim that has sent no
-- tool frame at all is judged from, so a turn that hangs before its first call reads as the turn
-- it is rather than as a null.
--
-- step_tool, step_call_id, step_args_bytes and step_args_hash name the call the agent is in: the
-- tool, the call id the start and end frames pair by — the only thing that tells two calls of one
-- tool with byte-identical arguments apart — and the LENGTH and a HASH of that call's arguments.
-- The arguments' own text is in no column: a shell command routinely holds a credential, and this
-- row is read by every operator surface. step_started_at and step_ended_at are when that call's
-- start and end were received, so a call still open has a start and no end, which is the shape a
-- stalled turn leaves behind.
--
-- Those six are overwritten by the next call and cleared by nothing, a turn's end least of all.
-- The recovery this record exists for ends the hung process, relaunches the claim and starts a
-- turn on the replacement, and a turn's start that cleared them would erase the one fact anybody
-- is looking for: which call was open when the agent stopped.
--
-- No backfill: every live claim records its next step, and one that has recorded none is judged
-- from the start of its next turn.
alter table claims add column step_at timestamptz;
alter table claims add column turn_started_at timestamptz;
alter table claims add column step_tool text not null default '';
alter table claims add column step_call_id text not null default '';
alter table claims add column step_args_bytes integer not null default 0;
alter table claims add column step_args_hash text not null default '';
alter table claims add column step_started_at timestamptz;
alter table claims add column step_ended_at timestamptz;

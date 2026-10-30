-- 0148_event_horizon.up.sql
--
-- The newest safe horizon of the event log any Dispatch process has proven (events.Horizon): the
-- highest event id at or below which every event is final. Finality is permanent, so a value one
-- process proved holds for every process after it, and a process that starts while an event insert
-- from before its boot is still open reads the log only above it rather than from id 0. safe_at is
-- the created_at of the event at safe, which tells a value of this log from one a reset of the log
-- (`truncate events restart identity`) left behind. One row, written as the horizon advances.
create table event_horizon (
  id smallint primary key check (id = 1),
  safe bigint not null,
  safe_at timestamptz not null,
  updated_at timestamptz not null default now()
);

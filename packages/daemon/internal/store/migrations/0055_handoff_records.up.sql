-- 0055_handoff_records.up.sql — the Dispatch handoff record a role's completion of its current
-- file-backed phase named (planning, implementing, testing, reviewing), and that record's version
-- among its phase's records, which the next worker's task names. Both are emptied with
-- handoff_commit when the role starts its next phase or round, and by a new generation. A row
-- written before this migration names no record; its last_handoff keeps the commit the role last
-- reported, which no record id equals.
alter table phases
  add column handoff_record text not null default '',
  add column handoff_version integer not null default 0 check (handoff_version >= 0);

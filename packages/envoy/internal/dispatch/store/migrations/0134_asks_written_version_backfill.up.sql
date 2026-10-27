-- 0134_asks_written_version_backfill.up.sql
--
-- Fills asks.written_version (0131) on the asks stored before 0131's trigger, with the trigger's
-- predicates:
--   - an approval ask: approval.version of its ask.opened event, the version the request named when
--     it opened, which the row's approval.version no longer holds once a later version moved it. The
--     lookup repeats the type list of events_ask_payload_id (0030), so the planner can read that
--     partial index. An approval ask with no such event keeps null;
--   - another ask with artifact_id set, anchor null and block_artifact_id null: the highest version
--     of its document created at or before the ask, as 0133 reads a comment's.
-- An ask the trigger already recorded keeps its value, and one with nothing to record keeps null
-- and is not rewritten. Setting only this column fires no trigger: asks' update triggers read an
-- update of the question, options or answer.
--
-- It takes asks EXCLUSIVE before it writes a row, as 0133 takes comments and for the same reason
-- (0089's): the table lock is taken while this holds nothing, so no write that locks two of these
-- rows in another order deadlocks with it. Its own migration, so the transaction holds asks' lock
-- alone.
lock table asks in exclusive mode;

with opened as (
  select a.id,
    case when e.payload -> 'approval' ->> 'version' ~ '^[1-9][0-9]{0,9}$'
      then (e.payload -> 'approval' ->> 'version')::bigint
    end as version
  from asks a
  cross join lateral (
    select payload from events
    where type in ('ask.opened', 'ask.answered', 'ask.resolved', 'ask.edited')
      and type = 'ask.opened'
      and payload ->> 'id' = a.id::text
    order by id
    limit 1
  ) e
  where a.kind = 'approval' and a.written_version is null
)
update asks a
set written_version = opened.version
from opened
where a.id = opened.id and opened.version <= 2147483647;

update asks a
set written_version = (
  select max(v.number) from artifact_versions v
  where v.artifact_id = a.artifact_id and v.created_at <= a.created_at
)
where a.kind <> 'approval'
  and a.artifact_id is not null
  and a.anchor is null
  and a.block_artifact_id is null
  and a.written_version is null
  and exists (
    select 1 from artifact_versions v
    where v.artifact_id = a.artifact_id and v.created_at <= a.created_at
  );

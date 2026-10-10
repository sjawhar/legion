-- 0091_decided_ask_wording.up.sql
-- LEGION-651: a decided ask's wording is fixed. Settlement used to write a document's edit of an
-- answered or resolved block ask's question, options, multiple or urgency into its row, so the row
-- held a wording nobody decided on beside the decision made on the one before it. Each such ask
-- gets back the wording its decision was given on: the previous wording of the first ask.edited
-- after its latest ask.answered or ask.resolved event, with no reopening (ask.opened) between. An
-- answer changed after the edit is a decision on the edited wording, so its ask is left as it is.
-- Only a row whose wording differs is written; a field an older event did not record is left as
-- the row has it.
--
-- The ask's block still holds the edited text, and the document's next settlement writes the
-- decided wording back into it. The migration marks no settlement owed: during a rolling deploy the
-- task it replaces is still running, and that task's settlement would write the block's edited text
-- into the row again.
with decided as (
  select a.id::text as ask_id,
    (select max(d.id) from events d
     where d.type in ('ask.answered', 'ask.resolved') and d.payload->>'id' = a.id::text) as decision
  from asks a
  where a.state in ('answered', 'resolved')
),
first_edit as (
  select distinct on (d.ask_id) d.ask_id, e.payload->'previous' as wording
  from decided d
  join events e on e.type = 'ask.edited' and e.payload->>'id' = d.ask_id and e.id > d.decision
  where not exists (
    select 1 from events o
    where o.type = 'ask.opened' and o.payload->>'id' = d.ask_id and o.id > d.decision and o.id < e.id)
  order by d.ask_id, e.id
)
update asks a
set question = f.wording->>'question',
    options = coalesce(f.wording->'options', a.options),
    multiple = coalesce((f.wording->>'multiple')::boolean, a.multiple),
    urgency = coalesce(f.wording->>'urgency', a.urgency)
from first_edit f
where a.id::text = f.ask_id
  and f.wording ? 'question'
  and (a.question, a.options, a.multiple, a.urgency) is distinct from
      (f.wording->>'question', coalesce(f.wording->'options', a.options),
       coalesce((f.wording->>'multiple')::boolean, a.multiple), coalesce(f.wording->>'urgency', a.urgency));

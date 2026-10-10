-- The decided asks 0088 gives back the wording their decision was given on: each answered or
-- resolved, with an ask.edited after its latest decision and no reopening between, whose wording
-- differs from that edit's previous one.
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
select count(*) from asks a join first_edit f on a.id::text = f.ask_id
where f.wording ? 'question'
  and (a.question, a.options, a.multiple, a.urgency) is distinct from
      (f.wording->>'question', coalesce(f.wording->'options', a.options),
       coalesce((f.wording->>'multiple')::boolean, a.multiple), coalesce(f.wording->>'urgency', a.urgency))

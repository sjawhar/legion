-- 0030_events_ask_payload_id.up.sql
-- Every ask read resolves the ask's canonical opened event id and its edit history from
-- events by payload->>'id'. This partial expression index serves those lookups.
-- The type list mirrors attachOpenedEventIDs (api/asks.go) and must change with it: the
-- planner only uses a partial index when the query's predicate is provably implied by
-- this one, which an exact IN-list gives and `type like 'ask.%'` does not.
create index if not exists events_ask_payload_id
  on events ((payload->>'id'))
  where type in ('ask.opened', 'ask.answered', 'ask.resolved', 'ask.edited');

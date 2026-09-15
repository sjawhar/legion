-- 0031_ask_options_array.up.sql
-- asks.options has been `jsonb not null default '[]'` since 0001, but rows and ask.* event
-- payloads written before options were normalised on input hold the JSON value null. The
-- wire shape is an array; rewrite the stored nulls and refuse any new one.
update asks set options = '[]'::jsonb where jsonb_typeof(options) <> 'array';

alter table asks add constraint asks_options_array check (jsonb_typeof(options) = 'array');

update events
set payload = jsonb_set(payload, '{options}', '[]'::jsonb)
where type like 'ask.%' and payload->'options' = 'null'::jsonb;

update events
set payload = jsonb_set(payload, '{previous,options}', '[]'::jsonb)
where type = 'ask.edited' and payload->'previous'->'options' = 'null'::jsonb;

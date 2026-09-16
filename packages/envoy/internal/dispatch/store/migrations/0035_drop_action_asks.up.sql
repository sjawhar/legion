-- 0035_drop_action_asks.up.sql
-- The `action` ask kind is gone: an ask is a `question` (its asker picks the options) or a
-- server-created `approval`. Every action ask already stores its Done / Can't options and
-- its answer on the row, so folding it into a question is a relabel of the row and of the
-- ask.* event payloads that carried the old kind; nothing else changes.
update asks set kind = 'question' where kind = 'action';

update events
set payload = jsonb_set(payload, '{kind}', '"question"'::jsonb)
where type like 'ask.%' and payload->>'kind' = 'action';

alter table asks drop constraint if exists asks_kind_check;
alter table asks add constraint asks_kind_check check (kind in ('question', 'approval'));

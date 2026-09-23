-- POST /api/v1/comments/{id}/reply stored its reply with reply_to set and ask_id left null,
-- while POST .../comments normalised the same reply up to the ask at the head of its thread.
-- Every ask read - the ask card, the Inbox row's thread, the waiting_on join - selects on
-- ask_id, so those rows sat outside the thread they belong to. Both paths normalise now; this
-- moves the rows already written.
with recursive ask_thread as (
  select c.id, c.ask_id
  from comments c
  where c.ask_id is not null
  union all
  select c.id, t.ask_id
  from comments c join ask_thread t on c.reply_to = t.id
)
update comments c
set ask_id = t.ask_id, reply_to = null
from ask_thread t
where c.id = t.id and c.ask_id is null;

-- Only an open ask has a turn to hold, and a reply that never carried an ask_id never carried
-- a turn either (comments_turn_requires_ask). These rows take the same turn 0028 gave every
-- other ask reply: a human's reply hands it to the agent, an agent's hands it back.
update comments c
set turn = case when c.author->>'kind' = 'user' then 'agent' else 'human' end
from asks a
where a.id = c.ask_id and a.state = 'open' and c.turn is null;

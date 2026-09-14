alter table comments add column turn text
  constraint comments_turn_value check (turn in ('human', 'agent')),
  add constraint comments_turn_requires_ask check (turn is null or ask_id is not null);

-- Only an open ask has a turn to hold. A human's reply hands it to the agent; an
-- agent's reply handed it back. Replies under closed asks keep no turn, and a
-- pre-0028 server still draining inserts ask replies with a null turn, which
-- readers treat as "human" until the ask's next reply.
update comments c
set turn = case when c.author->>'kind' = 'user' then 'agent' else 'human' end
from asks a
where a.id = c.ask_id and a.state = 'open';

-- POST /api/v1/comments/{id}/reply stored its reply with reply_to set and ask_id left null,
-- while POST .../comments normalised the same reply up to the ask at the head of its thread.
-- Every ask read - the ask card, the Inbox row's thread, the waiting_on join - selects on
-- ask_id, so those rows sat outside the thread they belong to. Both paths normalise now; this
-- moves the rows already written.
--
-- The moved set is named once and both statements below read it, so the backfill touches
-- exactly the rows this migration relocates: a reply that always carried its ask_id keeps
-- whatever turn its own write recorded, including the null a reply under a resolved ask
-- records and an ask reopened afterwards leaves in place. The runner wraps each migration
-- file in one transaction (store.applyMigration), so the table lives exactly that long.
--
-- comments.reply_to carries no acyclicity constraint, so the walk unions on the visited row
-- the way every other reply_to walk does (outbox/publisher.go's loadRootCommentAuthor): a
-- cyclic row repeats a pair already in the set and adds nothing, instead of spinning inside
-- the transaction that holds the migration advisory lock, where no Dispatch process boots.
create temporary table migrated_ask_replies on commit drop as
with recursive ask_thread as (
  select c.id, c.ask_id
  from comments c
  where c.ask_id is not null
  union
  select c.id, t.ask_id
  from comments c join ask_thread t on c.reply_to = t.id
)
select t.id, t.ask_id
from ask_thread t
join comments c on c.id = t.id
where c.ask_id is null;

update comments c
set ask_id = m.ask_id, reply_to = null
from migrated_ask_replies m
where c.id = m.id;

-- A reply that never carried an ask_id never carried a turn either
-- (comments_turn_requires_ask), and only an open ask has a turn to hold. The moved rows take
-- the same turn 0028 gave every other ask reply: a human's reply hands it to the agent, an
-- agent's hands it back.
update comments c
set turn = case when c.author->>'kind' = 'user' then 'agent' else 'human' end
from migrated_ask_replies m
join asks a on a.id = m.ask_id
where c.id = m.id and a.state = 'open';

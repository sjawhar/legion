-- 0120_messages_refs_owed_trigger.up.sql
-- INFRA-189: an issue message is owed a re-index (0117) when it is written and whenever its body
-- changes (nothing updates one today; 0077 notes the same). A direct message, which has no issue,
-- is never indexed, so it owes nothing. Its own migration, so the transaction holds messages' lock
-- alone (0077's precedent).
create function refs_owed_message() returns trigger language plpgsql as $$
begin
  if tg_op = 'UPDATE'
     and new.body is not distinct from old.body
     and new.issue_key is not distinct from old.issue_key then
    return null;
  end if;
  perform refs_owed_enqueue('message', new.id::text);
  return null;
end;
$$;

create trigger messages_refs_owed after insert or update of body, issue_key on messages
  for each row when (new.issue_key is not null) execute function refs_owed_message();

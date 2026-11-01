-- 0096_user_threads_asks_trigger.up.sql
-- LEGION-687: an ask a person asked, and one they answered, are threads they wrote in. Asking
-- gives the asker a row read through the ask. Answering (the answer route, a change of answer, and
-- settlement's restoration of a retracted ask) is a write like a reply: it moves the answerer's
-- read mark to the answer's own time and makes every other participant's row unread, the asker's
-- among them, naming the ask itself as the write, which the page reads as its answer. Its own
-- migration, so the transaction holds asks' lock alone (0075's precedent).
create function user_threads_ask() returns trigger language plpgsql as $$
begin
  if tg_op = 'INSERT' and new.author->>'kind' = 'user' then
    perform user_threads_record(
      new.id, 'ask', 'user', new.author->>'id', new.created_at, null, false,
      new.issue_key, new.artifact_id
    );
  end if;
  if new.answer->>'user' is not null and (tg_op = 'INSERT' or new.answer is distinct from old.answer) then
    perform user_threads_record(
      new.id, 'ask', 'user', new.answer->>'user', (new.answer->>'at')::timestamptz, new.id, true,
      new.issue_key, new.artifact_id
    );
  end if;
  return null;
end;
$$;

create trigger user_threads_ask after insert or update of answer on asks
  for each row execute function user_threads_ask();

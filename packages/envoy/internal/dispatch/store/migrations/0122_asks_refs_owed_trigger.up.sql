-- 0122_asks_refs_owed_trigger.up.sql
-- INFRA-189: an ask is owed a re-index (0120) when it is written and whenever what the index reads
-- of it changes: its question, or what decides the document its `#id` resolves against
-- (refs.ScopeOf: its block's document, its anchor's document, its owning document, its issue). An
-- answer is not indexed, and a refresh of the anchor's position within its document owes nothing.
--
-- An ask that settlement makes for a question block, or whose block moves, changes its document's
-- question map (refs.QuestionMap), so a `#id` written before the question's row existed - another
-- question of the same edit saying "see #q2", which settlement indexes before it makes q2's row -
-- resolves only now: the document is owed as a scope (both documents, when the block moves from one
-- to another). Its own migration, so the transaction holds asks' lock alone (0076's precedent).
create function refs_owed_ask() returns trigger language plpgsql as $$
begin
  if tg_op = 'INSERT'
     or new.question is distinct from old.question
     or new.block_artifact_id is distinct from old.block_artifact_id
     or (new.anchor->>'artifact_id') is distinct from (old.anchor->>'artifact_id')
     or new.artifact_id is distinct from old.artifact_id
     or new.issue_key is distinct from old.issue_key then
    perform refs_owed_enqueue('ask', new.id::text);
  end if;
  if tg_op = 'INSERT'
     or new.block_id is distinct from old.block_id
     or new.block_artifact_id is distinct from old.block_artifact_id then
    if new.block_artifact_id is not null then
      perform refs_owed_enqueue('scope', new.block_artifact_id::text);
    end if;
    if tg_op = 'UPDATE' and old.block_artifact_id is not null
       and old.block_artifact_id is distinct from new.block_artifact_id then
      perform refs_owed_enqueue('scope', old.block_artifact_id::text);
    end if;
  end if;
  return null;
end;
$$;

create trigger asks_refs_owed after insert or update of question, block_id, block_artifact_id, anchor, artifact_id, issue_key on asks
  for each row execute function refs_owed_ask();

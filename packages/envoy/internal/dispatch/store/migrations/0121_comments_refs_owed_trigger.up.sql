-- 0121_comments_refs_owed_trigger.up.sql
-- INFRA-189: a comment is owed a re-index (0120) when it is written and whenever what the index
-- reads of it changes: its body, or what decides the document its `#id` resolves against
-- (refs.ScopeOf: its anchor's document, its owning document, the ask it replies to, the comment it
-- replies to, its issue). An update that leaves all of those as they were - a resolution, a refresh
-- of the anchor's position within its document - owes nothing. Its own migration, so the
-- transaction holds comments' lock alone, for milliseconds (0075's precedent).
create function refs_owed_comment() returns trigger language plpgsql as $$
begin
  if tg_op = 'UPDATE'
     and new.body is not distinct from old.body
     and new.reply_to is not distinct from old.reply_to
     and (new.anchor->>'artifact_id') is not distinct from (old.anchor->>'artifact_id')
     and new.artifact_id is not distinct from old.artifact_id
     and new.ask_id is not distinct from old.ask_id
     and new.issue_key is not distinct from old.issue_key then
    return null;
  end if;
  perform refs_owed_enqueue('comment', new.id::text);
  return null;
end;
$$;

create trigger comments_refs_owed after insert or update of body, reply_to, anchor, artifact_id, ask_id, issue_key on comments
  for each row execute function refs_owed_comment();

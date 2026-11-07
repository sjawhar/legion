-- 20261103205038_comments_refs_grammar_trigger.up.sql
-- A comment any build writes is owed an index with the whole grammar (20261103205037's
-- refs_grammar_owed). It enqueues on every write of what the index reads, whatever that text
-- says: no cheap SQL predicate finds a bare issue key, a `#id` or a short id, which is what 0123's
-- `like '%#b-%'` could do for the block grammar, and 0122's document trigger is the precedent for
-- a trigger that enqueues every write. A write this build makes deletes the row in its own
-- transaction (refs.replaceRefs), so only a write of an older build stays owed.
-- What the index reads is the body and the document a `#id` in it resolves against
-- (refs.ScopeOf, refs.commentScope): the anchor's artifact_id, the document or the ask that owns
-- the comment, the comment it replies to, and its issue, whose spec a comment naming none belongs
-- to. A column that moves the scope of the ask or the parent comment it hangs on - that ask's
-- block, say - owes the document's whole scope instead (20261103205039's
-- asks_refs_grammar_scope_update), which re-indexes this comment with the rest of that scope.
-- The update is a trigger of its own with a WHEN of its own because `update of ... anchor` fires
-- on every anchor write, and the two anchor writes a settlement makes change no edge: docs/marks.go
-- rewrites anchor.quote and anchor.orphaned as a quote moves, and docs/anchor_blocks.go writes
-- anchor.block_id. Neither calls refs.replaceRefs, so without the WHEN each such row stays owed
-- until a drain re-indexes a text that has not changed. Only the anchor's artifact_id is read, so
-- only that part of it is compared. The insert is a trigger of its own because a WHEN clause of an
-- insert trigger may not name OLD.
-- Its own migration, so the transaction holds comments' lock alone (0123's precedent).
create trigger comments_refs_grammar_owed_insert after insert on comments
  for each row execute function refs_grammar_owe('comment');
create trigger comments_refs_grammar_owed_update
  after update of body, reply_to, anchor, artifact_id, ask_id, issue_key on comments
  for each row when (old.body is distinct from new.body
                     or old.reply_to is distinct from new.reply_to
                     or old.anchor->>'artifact_id' is distinct from new.anchor->>'artifact_id'
                     or old.artifact_id is distinct from new.artifact_id
                     or old.ask_id is distinct from new.ask_id
                     or old.issue_key is distinct from new.issue_key)
  execute function refs_grammar_owe('comment');

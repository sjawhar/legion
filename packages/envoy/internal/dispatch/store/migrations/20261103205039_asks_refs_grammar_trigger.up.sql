-- 20261103205039_asks_refs_grammar_trigger.up.sql
-- An ask any build writes is owed an index with the whole grammar (20261103205037's
-- refs_grammar_owed) on every write of what the index reads, as the comments trigger before it is
-- and for the same reason: no SQL predicate finds a bare issue key, a `#id` or a short id. What
-- the index reads is the question and the document a `#id` in it resolves against (refs.ScopeOf,
-- refs.askScope): the document the ask's block is on, else the anchor's artifact_id, else the
-- document that owns it, else its issue, whose spec it belongs to then.
-- The update is a trigger of its own with a WHEN of its own, as the comments trigger's is, because
-- `update of ... anchor` fires on every anchor write and the anchor writes a settlement makes
-- change no edge: docs/marks.go rewrites anchor.quote and anchor.orphaned as a quote moves, and
-- docs/anchor_blocks.go writes anchor.block_id. Neither calls refs.replaceRefs, so without the
-- WHEN each such row stays owed until a drain re-indexes a text that has not changed. Only the
-- anchor's artifact_id is read, so only that part of it is compared. The insert is a trigger of
-- its own because a WHEN clause of an insert trigger may not name OLD.
-- block_id is read by no scope: it keys the question map of the document the block is on
-- (refs.QuestionMap), which is what the two triggers below owe that document's scope for, and the
-- drain of that scope re-indexes this ask with every other source of it.
--
-- The last two triggers owe that document's whole scope rather than the ask: an ask that gains or
-- changes its block is the settlement of a question block, and every `#id` naming that block,
-- written anywhere in the document's scope before it settled, resolves from then on. They are two
-- triggers for the reason the pair above is.
-- All four ride in one migration: they are triggers of one table, so one transaction holds asks'
-- lock once (0124's precedent, whose rule is one table's lock per migration).
create trigger asks_refs_grammar_owed_insert after insert on asks
  for each row execute function refs_grammar_owe('ask');
create trigger asks_refs_grammar_owed_update
  after update of question, anchor, block_artifact_id, artifact_id, issue_key on asks
  for each row when (old.question is distinct from new.question
                     or old.anchor->>'artifact_id' is distinct from new.anchor->>'artifact_id'
                     or old.block_artifact_id is distinct from new.block_artifact_id
                     or old.artifact_id is distinct from new.artifact_id
                     or old.issue_key is distinct from new.issue_key)
  execute function refs_grammar_owe('ask');
create trigger asks_refs_grammar_scope_insert after insert on asks
  for each row when (new.block_artifact_id is not null) execute function refs_grammar_owe_ask_scope();
create trigger asks_refs_grammar_scope_update after update of block_id, block_artifact_id on asks
  for each row when (old.block_id is distinct from new.block_id
                     or old.block_artifact_id is distinct from new.block_artifact_id)
  execute function refs_grammar_owe_ask_scope();

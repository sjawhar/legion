-- 20261103205039_asks_refs_grammar_trigger.up.sql
-- An ask any build writes is owed an index with the whole grammar (20261103205037's
-- refs_grammar_owed), unconditionally, as the comments trigger before it is and for the same
-- reason: no SQL predicate finds a bare issue key, a `#id` or a short id. Its columns are the
-- ones the index reads: the question, and the block_id, block_artifact_id and anchor that say
-- which document a `#id` in it resolves against (refs.ScopeOf).
--
-- The second and third triggers owe that document's whole scope rather than the ask: an ask that
-- gains or changes its block is the settlement of a question block, and every `#id` naming that
-- block, written anywhere in the document's scope before it settled, resolves from then on.
-- The insert and the update are two triggers because a WHEN clause of an insert trigger may not
-- name OLD.
-- All three ride in one migration: they are triggers of one table, so one transaction holds asks'
-- lock once (0124's precedent, whose rule is one table's lock per migration).
create trigger asks_refs_grammar_owed after insert or update of question, block_id, block_artifact_id, anchor on asks
  for each row execute function refs_grammar_owe('ask');
create trigger asks_refs_grammar_scope_insert after insert on asks
  for each row when (new.block_artifact_id is not null) execute function refs_grammar_owe_ask_scope();
create trigger asks_refs_grammar_scope_update after update of block_id, block_artifact_id on asks
  for each row when (old.block_id is distinct from new.block_id
                     or old.block_artifact_id is distinct from new.block_artifact_id)
  execute function refs_grammar_owe_ask_scope();

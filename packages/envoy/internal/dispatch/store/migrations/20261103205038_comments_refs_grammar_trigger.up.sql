-- 20261103205038_comments_refs_grammar_trigger.up.sql
-- A comment any build writes is owed an index with the whole grammar (20261103205037's
-- refs_grammar_owed). It enqueues unconditionally: no cheap SQL predicate finds a bare issue key,
-- a `#id` or a short id, which is what 0123's `like '%#b-%'` could do for the block grammar, and
-- 0122's document trigger is the precedent for a trigger that enqueues every write. A write this
-- build makes deletes the row in its own transaction (refs.replaceRefs), so only a write of an
-- older build stays owed.
-- Its columns are the ones the index reads: the body, and the reply_to and anchor that say which
-- document a `#id` in it resolves against (refs.ScopeOf).
-- Its own migration, so the transaction holds comments' lock alone (0123's precedent).
create trigger comments_refs_grammar_owed after insert or update of body, reply_to, anchor on comments
  for each row execute function refs_grammar_owe('comment');

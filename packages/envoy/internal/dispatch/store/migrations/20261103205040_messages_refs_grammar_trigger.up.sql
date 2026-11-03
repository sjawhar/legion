-- 20261103205040_messages_refs_grammar_trigger.up.sql
-- An issue message any build writes is owed an index with the whole grammar (20261103205037's
-- refs_grammar_owed), unconditionally among the messages the index reads: a direct message is
-- indexed by nothing, so it owes nothing, and that is the one predicate here, as it is 0125's.
-- Its own migration, so the transaction holds messages' lock alone (0125's precedent).
create trigger messages_refs_grammar_owed after insert or update of body on messages
  for each row when (new.issue_key is not null) execute function refs_grammar_owe('message');

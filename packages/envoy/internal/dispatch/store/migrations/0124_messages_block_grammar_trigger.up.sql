-- 0124_messages_block_grammar_trigger.up.sql
-- An issue message whose body holds `#b-`, written by any build, is owed an index with the block
-- grammar (0120's block_grammar_owed). A direct message is indexed by nothing, so it owes nothing.
-- Its own migration, so the transaction holds messages' lock alone (0077's precedent).
create trigger messages_block_grammar_owed after insert or update of body on messages
  for each row when (new.issue_key is not null and new.body like '%#b-%')
  execute function block_grammar_owe('message');

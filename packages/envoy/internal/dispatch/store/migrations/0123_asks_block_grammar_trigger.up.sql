-- 0123_asks_block_grammar_trigger.up.sql
-- An ask whose question holds `#b-`, written by any build, is owed an index with the block grammar
-- (0120's block_grammar_owed). Its own migration, so the transaction holds asks' lock alone
-- (0076's precedent).
create trigger asks_block_grammar_owed after insert or update of question on asks
  for each row when (new.question like '%#b-%') execute function block_grammar_owe('ask');

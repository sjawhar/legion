-- 0123_comments_block_grammar_trigger.up.sql
-- A comment whose body holds `#b-`, written by any build, is owed an index with the block grammar
-- (0121's block_grammar_owed). Its own migration, so the transaction holds comments' lock alone
-- (0075's precedent).
create trigger comments_block_grammar_owed after insert or update of body on comments
  for each row when (new.body like '%#b-%') execute function block_grammar_owe('comment');

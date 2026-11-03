-- 0126_block_grammar_seed.up.sql
-- Owes an index with the block grammar to every comment, ask and issue message already stored
-- whose text holds `#b-`: the texts rebuild-refs indexes (refs.RebuildAll), read with the
-- predicates of 0123-0125's triggers. Those triggers are live by now, so a write that lands while
-- this runs is owed by its trigger, and a row the trigger already wrote keeps its enqueued_at.
-- Reads comments, asks and messages (ACCESS SHARE) and writes only block_grammar_owed.
insert into block_grammar_owed (kind, id)
select 'comment', id from comments where body like '%#b-%'
union all
select 'ask', id from asks where question like '%#b-%'
union all
select 'message', id from messages where issue_key is not null and body like '%#b-%'
on conflict (kind, id) do nothing;

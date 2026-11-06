-- 20261103070013 adds one column to agent_tokens with a constant default, so every existing row
-- takes that default without being rewritten and the column's check, which the default satisfies,
-- refuses none of them. It cannot name the column, which does not exist when the census is taken.
select 0;

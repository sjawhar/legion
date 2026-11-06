-- 20261105070440 adds one column to pull_request_heads with a constant default, so every stored
-- row takes that default without being rewritten and none is refused, and it replaces one
-- partial index, which reads the table's rows to build but changes none. It cannot name the
-- column, which does not exist when the census is taken.
select 0;

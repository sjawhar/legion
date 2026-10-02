-- 0055 creates an empty table (broadcast_idempotency_keys) with a foreign key to broadcasts and
-- nothing else: no row that exists before it can violate anything it adds, and no row is
-- rewritten. The one lock it takes on an existing table is the foreign key's SHARE ROW EXCLUSIVE
-- on broadcasts, which the census reports with that table's row count and size.
select 0;

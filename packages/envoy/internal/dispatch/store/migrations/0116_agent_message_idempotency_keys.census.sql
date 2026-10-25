-- 0116 creates an empty table (agent_message_idempotency_keys) with a foreign key to messages and
-- nothing else: no row that exists before it can violate anything it adds, and no row is
-- rewritten. The one lock it takes on an existing table is the foreign key's SHARE ROW EXCLUSIVE
-- on messages, which the census reports with that table's row count and size.
select 0;

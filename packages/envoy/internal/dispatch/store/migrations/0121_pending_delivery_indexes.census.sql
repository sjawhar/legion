-- 0121 creates two partial indexes (message_deliveries_pending, comment_deliveries_pending) and
-- nothing else: no row that exists before it can violate anything it adds, and no row is
-- rewritten. What it locks, and the rows of those tables, the census reports on its own.
select 0;

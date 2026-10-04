-- 0054 adds three nullable columns to message_deliveries, two checks over those columns alone
-- (both pass on a row where all three are null) and a unique index over the rows whose
-- accepted_at is set, which no row that exists before it is: no existing row can violate
-- anything it adds, and none is rewritten.
select 0;

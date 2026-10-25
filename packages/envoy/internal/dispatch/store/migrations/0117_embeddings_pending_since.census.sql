-- Two columns added with a constant default or none, which refuse no row. The update writes only
-- pending_since, the column this migration adds, so no value an earlier release stored changes.
select 0;

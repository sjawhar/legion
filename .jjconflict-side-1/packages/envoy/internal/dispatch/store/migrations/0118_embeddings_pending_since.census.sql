-- Two columns added with a constant default or none, which refuse no row. The updates write only
-- pending_since and dead_model, the columns this migration adds, so no value an earlier release
-- stored changes.
select 0;

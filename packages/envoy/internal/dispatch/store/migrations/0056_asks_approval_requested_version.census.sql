-- 0056 backfills a required field from approval.version. Migration 0053 already refuses every
-- stored approval shape that cannot supply that source value, so no existing row can block it.
select 0;

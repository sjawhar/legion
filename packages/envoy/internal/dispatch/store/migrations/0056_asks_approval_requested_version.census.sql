-- 0056 backfills requested_version from approval.version and adds the insert trigger that gives
-- a rolling older binary the same value. Migration 0053 already refuses every stored approval
-- shape that cannot supply that source value, so no existing row can block it.
select 0;

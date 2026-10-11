-- When a pod enrollment's renewal first carried its pod's projected token (enroll.Service.Renew).
-- Every pod row starts null, whether this migration found it or enroll.Service.Create inserted it,
-- and renews without a token until its first renewal with one sets it; from then on every renewal
-- must carry the token. Null for box and host enrollments. Additive and nullable, safe on a fresh
-- or already-migrated database.
alter table enrollments add column if not exists pod_token_renewed_at timestamptz;

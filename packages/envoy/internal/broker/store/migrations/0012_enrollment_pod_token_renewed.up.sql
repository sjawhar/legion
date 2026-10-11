-- When a pod enrollment's renewal first had to carry its pod's projected token (enroll.Service.Renew).
-- Create sets it on every pod row it inserts, so a pod enrolled by a broker past this migration must
-- send the token from its first renewal. A pod row that exists when this migration runs, made by a
-- broker that predates the token check, starts null and renews without a token until its first
-- renewal with one sets it. Null for box and host enrollments. Additive and nullable, safe on a fresh
-- or already-migrated database.
alter table enrollments add column if not exists pod_token_renewed_at timestamptz;

-- When a pod enrollment first renewed carrying its pod's projected token (enroll.Service.Renew).
-- Once set, every later renewal of that enrollment must carry a token for the pod UID and
-- service account it enrolled with; while null, a renewal without one is still accepted, for a
-- pod whose agent-secrets renew predates the token. Null for box and host enrollments. Additive
-- and nullable, safe on a fresh or already-migrated database.
alter table enrollments add column if not exists pod_token_renewed_at timestamptz;

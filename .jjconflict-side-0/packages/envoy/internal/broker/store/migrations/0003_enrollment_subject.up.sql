-- The verified Kubernetes service-account subject (system:serviceaccount:<namespace>:<name>) a pod
-- enrollment's projected token carried, which a pod rule's service_account is matched against.
-- Null for box and host enrollments. Additive and nullable, safe on a fresh or already-migrated
-- database.
alter table enrollments add column if not exists subject text;

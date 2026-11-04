-- packages/envoy/internal/broker/store/migrations/0026_launcher_credential_pod_login.up.sql
-- A machine login is per pod, not per account. Migration 0015 gave every subject one live
-- credential, so a second pod running as an account took the first pod's row over: its key, its
-- public JWK and (since 0025) its pod. That is right for a launcher, which logs in from one place
-- and whose token names no pod of its own, and wrong for an account whose pods hold logins of
-- their own, where several pods run at once and each must keep its own key.
--
-- So the uniqueness becomes two: one live credential per subject among the logins bound to no pod
-- (a `kubectl create token` token, every launcher's today), and one per subject and pod among the
-- logins a pod's own projected token made. Every row this migration finds has a null pod_uid --
-- 0025 added the column empty and nothing has written it yet -- so each goes in the first index
-- and no row conflicts.
--
-- enroll.Service.IssueTokenLogin names one of the two as its conflict target by whether the login
-- carries a pod, so a pod logging in again from the same pod still moves its own row rather than
-- inserting a second, and a launcher's login still moves its account's one row.
--
-- A launcher's replaced pod therefore holds a credential its predecessor's enrollments were not
-- made under. enroll.Service.Revoke's sameLauncher gives it those enrollments anyway -- the same
-- subject, the same service, and a pod of an account BROKER_SERVICES binds to that service --
-- since otherwise one deploy, which replaces every launcher pod at once, would leave every
-- enrollment they made running to its lease with nothing able to revoke it.
--
-- The index of 0015 goes: it would refuse the second pod's insert before either of these is
-- consulted.
drop index launcher_credentials_live_subject;
create unique index launcher_credentials_live_account_login on launcher_credentials (subject)
  where subject is not null and pod_uid is null and revoked_at is null;
create unique index launcher_credentials_live_pod_login on launcher_credentials (subject, pod_uid)
  where subject is not null and pod_uid is not null and revoked_at is null;

-- packages/envoy/internal/broker/store/migrations/0029_launcher_credential_pod_login.up.sql
-- A machine login is to become per pod, not per account. Migration 0015 gave every subject one
-- live credential, so a second pod running as an account takes the first pod's row over: its key,
-- its public JWK and (since 0028) its pod. That is right for a launcher, which logs in from one
-- place and whose token names no pod of its own, and wrong for an account whose pods hold logins
-- of their own, where several pods run at once and each must keep its own key.
--
-- So the uniqueness becomes two: one live credential per subject among the logins bound to no pod
-- (a `kubectl create token` token, every launcher's today), and one per subject and pod among the
-- logins a pod's own projected token made. Every row this migration finds has a null pod_uid --
-- 0028 added the column empty and nothing has written it yet -- so each goes in the first index
-- and no row conflicts.
--
-- **This migration adds those two indexes and drops nothing.** 0015's
-- launcher_credentials_live_subject stays for one release, because the broker runs more than one
-- pod and a roll of it serves both generations at once (docs/site/.../broker/operate.md, "Running
-- more than one pod": a rolling deploy counts one pod more than the Deployment runs, and
-- cmd/broker/multiprocess_test.go runs three or four for the length of every roll). The previous
-- broker's upsert names `on conflict (subject) where subject is not null and revoked_at is null`,
-- which Postgres resolves only against that index: neither index added here is implied by that
-- predicate, so dropping it here would fail every token login and every machine token exchange
-- on the old pods with 42P10 for the length of the roll. Nothing in the deployment can keep the
-- two generations apart -- a migration that refused while old replicas were live would refuse
-- every roll, since the new pod migrates at boot precisely while the old ones still serve.
--
-- So while that index is here, one subject still holds one live credential and
-- enroll.Service.IssueTokenLoginTx keeps the previous broker's conflict target, which is the
-- behaviour this release ships: a replaced pod takes the account's row over, as it does today.
-- The follow-up migration of the next release drops launcher_credentials_live_subject, and the
-- per-pod login starts there -- IssueTokenLoginTx reads which of the two schemas it is on and
-- names the conflict target that schema has, so each generation is right on either.
--
-- enroll.Service.Revoke's sameLauncher, which gives a replaced launcher pod its predecessor's
-- enrollments, is written for that release: until the drop no launcher pod holds a row of its
-- own to need it, and with it no deploy, which replaces every launcher pod at once, leaves their
-- enrollments running to their leases with nothing able to revoke them.
create unique index launcher_credentials_live_account_login on launcher_credentials (subject)
  where subject is not null and pod_uid is null and revoked_at is null;
create unique index launcher_credentials_live_pod_login on launcher_credentials (subject, pod_uid)
  where subject is not null and pod_uid is not null and revoked_at is null;

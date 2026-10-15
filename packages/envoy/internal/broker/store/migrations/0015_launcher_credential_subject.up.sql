-- packages/envoy/internal/broker/store/migrations/0015_launcher_credential_subject.up.sql
-- A service logs in with its own Kubernetes ServiceAccount token (enroll.Service.IssueTokenLogin),
-- and the launcher credential that login issues records the token's verified subject,
-- system:serviceaccount:<namespace>:<name>. Such a credential has no credential-request record
-- behind it (record_id stays null) and always names its service, which the check constraint holds:
-- AuthenticateLauncher authenticates it while BROKER_SERVICE_LOGINS still names its subject for that
-- service, and a person's machine login, whose subject is null, by its approved record as before.
-- Each subject holds one live credential, which every login moves to the login's key and expiry, so
-- the partial unique index below is what a login locks and updates.
alter table launcher_credentials add column subject text,
  add constraint launcher_credentials_subject_names_service check (subject is null or (service is not null and record_id is null));
create unique index launcher_credentials_live_subject on launcher_credentials (subject)
  where subject is not null and revoked_at is null;

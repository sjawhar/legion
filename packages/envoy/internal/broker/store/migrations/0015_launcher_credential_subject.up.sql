-- packages/envoy/internal/broker/store/migrations/0015_launcher_credential_subject.up.sql
-- A service logs in with its own Kubernetes ServiceAccount token (machine.Service.TokenLogin), and
-- the launcher credential that login issues records the token's verified subject,
-- system:serviceaccount:<namespace>:<name>. Such a credential has no credential-request record
-- behind it (record_id stays null): AuthenticateLauncher authenticates it while BROKER_SERVICE_LOGINS
-- still names its subject for its service, and a person's machine login, whose subject is null, by
-- its approved record as before. Each subject holds one live credential, which every login moves to
-- the login's key and expiry, so the partial unique index below is what a login locks and updates.
alter table launcher_credentials add column subject text;
create unique index launcher_credentials_live_subject on launcher_credentials (subject)
  where subject is not null and revoked_at is null;

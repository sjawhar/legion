-- packages/envoy/internal/broker/store/migrations/0002_launcher_request_service.up.sql
-- The optional service name a launcher_credential_requests row was opened for (Task 12's
-- launcher.Service.Request): non-null means Reconcile mints the eventual credential with a nil
-- operator and this service instead of the reverse. Additive and nullable, safe on a fresh or
-- already-migrated database.
alter table launcher_credential_requests add column if not exists service text;

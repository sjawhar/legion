-- packages/envoy/internal/broker/store/migrations/0020_vm_enrollment_kind.up.sql
-- An enrollment may be of kind vm: a Google Cloud VM that proves its service account with the
-- identity token Google signs for it, which its metadata server serves; its runtime id names the
-- account with the VM's project, zone and instance id. The check only
-- widens, from session and pod to session, pod and vm, and rewrites no row, so the build before
-- this one, which writes session and pod alone, serves unchanged over the migrated database while
-- a rolling deploy runs both. Re-adding the constraint validates the rows enrollments holds under
-- the runner's one transaction; every stored row is session or pod, which the new check admits.
alter table enrollments drop constraint enrollments_kind_check;
alter table enrollments add constraint enrollments_kind_check check (kind in ('session', 'pod', 'vm'));

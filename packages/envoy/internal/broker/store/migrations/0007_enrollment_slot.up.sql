-- packages/envoy/internal/broker/store/migrations/0007_enrollment_slot.up.sql
-- A pod enrollment's slot names one of several independent identities inside the same pod (a
-- Legion role and its generation, `<role>-g<generation>`, which the trusted launcher derives); ''
-- is the one identity every box, host and single-identity pod enrollment has. A live enrollment
-- is unique per launcher credential, runtime id and slot, so two slots of one pod each hold their
-- own key and grants while a retry in one slot still finds its own row.
--
-- Forward-only: an older broker binary's enrollment conflict lookup reads one live row per
-- runtime id, which is no longer unique once a pod holds two slots, so rolling the binary back
-- past this migration is unsafe after any slotted enrollment exists.
alter table enrollments add column slot text not null default '';
create unique index enrollments_live_runtime_slot
    on enrollments (launcher_credential_id, runtime_id, slot)
    where revoked_at is null;
drop index enrollments_live_runtime;

-- packages/envoy/internal/broker/store/migrations/0009_request_secrets_delivery_default.up.sql
-- A secret's policy is its owner and tier tags, and every granted value is injected into the
-- requesting command, so the broker no longer writes or reads request_secrets.delivery. The column
-- stays, defaulting every new row to 'inject', so a broker binary from before this migration,
-- which still reads it on every value release, keeps working if it is rolled back to.
alter table request_secrets alter column delivery set default 'inject';

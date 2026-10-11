-- packages/envoy/internal/broker/store/migrations/0006_approval_by_login.up.sql
-- A human decides a credential request with their Dispatch login, not a WebAuthn
-- assertion, so the broker keeps no approver keys, seeds or ceremonies, and a decision event
-- records the login that made it instead of an assertion. No deployment holds rows in these
-- tables: production has never run the broker, and development databases are rebuilt.
drop table approver_keys;
drop table approver_key_seeds;
drop table webauthn_ceremonies;
alter table credential_request_events drop column assertion;
-- login is the Dispatch login that approved or denied the record, as Dispatch reported it; null
-- for an event no human decided (expired, cancelled).
alter table credential_request_events add column login text;

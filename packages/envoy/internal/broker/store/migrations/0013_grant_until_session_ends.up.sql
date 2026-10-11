-- packages/envoy/internal/broker/store/migrations/0013_grant_until_session_ends.up.sql
-- A grant lasts until its session ends, or for a shorter lifetime when its request asks for one
-- (the signed request object's lifetime_seconds). A request that asks for none records no lifetime,
-- and its grant no expiry: the enrollment's end revokes it (enroll.endEnrollment), and nothing else
-- does but a person's revoke. A request and grant written before this migration keep the lifetime
-- and expiry they were given. Every read that asks whether a grant is live treats a null expiry as
-- one that has not passed.
--
-- Dropping not null rewrites no row and changes only the catalog.
alter table requests alter column lifetime_seconds drop not null;
alter table grants alter column expires_at drop not null;

-- 0007_controller.up.sql — each project's operator-launched controller: the hash of the capability
-- `legion controller start` was last issued, how many have been issued, and the session that
-- registered with the current one. A mint replaces the hash and clears the registration.
create table controllers (
  project text primary key,
  capability_hash bytea not null,
  generation bigint not null check (generation > 0),
  session text not null default '',
  secret_hash bytea,
  registered_at timestamptz,
  check ((session = '') = (secret_hash is null) and (secret_hash is null) = (registered_at is null)));

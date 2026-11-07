-- packages/envoy/internal/broker/store/migrations/0031_airlock_placeholders.up.sql
-- A secret the airlock's rules name is never released as its value: the session is given a
-- placeholder, and the broker's proxy port puts the real value into the requests the rules
-- allow. This table is where those placeholders live, one row per (grant, name, slot): the slot
-- says which value of the secret the placeholder stands for (value, the three aws- slots of a
-- stored key pair, github:<owner> for one installation, azure-federated), because one secret can
-- release several values and each needs its own placeholder.
--
-- Both the hash and the plaintext are columns, which a reader will ask about:
--   - placeholder_sha256 is the primary key, so the proxy's per-connection lookup carries only a
--     hash in its query. The plaintext a command holds therefore never reaches a statement log,
--     pg_stat_statements or a database error, which is where a proxy password keyed by its own
--     text would end up.
--   - placeholder is stored all the same, because the proxy must know a grant's other
--     placeholders to swap them into a request, and because a second release of one grant must
--     hand back the placeholders the first one did rather than growing a row per release, which
--     unique (grant_id, name, slot) is what makes idempotent.
-- A placeholder is not a credential on its own: it works only as the proxy password of a live
-- grant, it carries no secret's value, and it opens nothing once the grant ends.
--
-- The rows go with the grant: on delete cascade covers a deleted grant, and the request sweeper
-- deletes the rows of grants that are revoked or expired, so the table does not grow without
-- bound. expires_at is the grant's own expiry, null for a grant that lasts until its session
-- ends, as grants.expires_at is.
--
-- The two reads this table serves are indexed by the constraints alone: the proxy's lookup by
-- the primary key, and the release's and the sweeper's reads by grant, which the unique
-- constraint's index serves because grant_id leads it. A separate index on (grant_id) would
-- repeat that index's leading column and be paid for on every write, so there is none.
create table if not exists airlock_placeholders (
  placeholder_sha256 text primary key,
  placeholder text not null,
  grant_id uuid not null references grants(id) on delete cascade,
  name text not null,
  slot text not null,
  created_at timestamptz not null default now(),
  expires_at timestamptz,
  unique (grant_id, name, slot)
);

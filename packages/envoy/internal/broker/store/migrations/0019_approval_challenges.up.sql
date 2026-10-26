-- packages/envoy/internal/broker/store/migrations/0019_approval_challenges.up.sql
-- The approval challenges (internal/broker/approval): one row per pending record a person has
-- been sent to the broker's own approval page for, which the person's own Google sign-in spends.
-- The row is what the sign-in is compared against — the record it decides, the login the email
-- must match, and the hash of a machine login's typed code, never the code itself — and, once
-- spent, the broker's own record that the decision carried that person's sign-in: used_at with
-- the issuer and the subject (sub) of the ID token it came back with, which nothing but a sign-in
-- through the broker's own OAuth client can write, and the token's auth_time, when the person
-- last authenticated to the issuer (null when the token carried none).
--
-- A spent row is kept; an unspent one that expired is removed an hour later (approval.Open), so
-- the table holds the live challenges and the history of who signed in rather than every link
-- anyone opened and abandoned. The index on expires_at serves that removal.
--
-- The record is referenced rather than copied: the page shows what credential_requests holds now,
-- and a challenge can only ever have been opened for a record this broker already had.
create table approval_challenges (
  id text primary key,
  record_id text not null references credential_requests(id),
  kind text not null check (kind in ('agent_secret','launcher_credential')),
  approver text not null,
  code_hash bytea,
  created_at timestamptz not null default now(),
  expires_at timestamptz not null,
  used_at timestamptz,
  signed_in_issuer text,
  signed_in_subject text,
  signed_in_auth_time timestamptz
);
create index approval_challenges_expires on approval_challenges (expires_at) where used_at is null;
create index approval_challenges_record on approval_challenges (record_id);

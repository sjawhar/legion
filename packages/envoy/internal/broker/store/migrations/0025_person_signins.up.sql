-- packages/envoy/internal/broker/store/migrations/0025_person_signins.up.sql
-- One sign-in of one person on one machine: the person `agent-secrets login` signed in as, the
-- RFC 7638 thumbprint of the box key that command made and keeps with Google's refresh token, and
-- the Google account of the machine it was made on. It is what the RFC 8693 token exchange holds
-- a person's trade to: the first trade of a person and a box key writes the row, every later
-- trade of that pair from that same machine is the same sign-in used again, and a trade whose
-- box key the person has revoked, or whose box key belongs to a sign-in made on another
-- machine, is refused. It is in Postgres, and not in each process, because a person revokes a
-- sign-in on one replica and every replica must refuse it from that moment, and because a sign-in
-- outlives any process: a box that has not traded for a week signs in with the key it already
-- holds.
--
-- A person holds at most one row per box key, live or revoked, which the unique index says. A
-- revoked row keeps that pair's slot forever: it is what refuses that key for good, so the
-- revoke a person made is not undone by signing in again with the very key they revoked, and the
-- insert's own conflict is the one place a racing revoke and trade meet. The person signing in
-- on a second box is a second thumbprint and a row of its own, which is what makes their other
-- sign-ins go on working when one is revoked. The machine's account never changes on a row: the
-- launcher credential a sign-in holds is keyed by the person, the machine and the key together,
-- so a row that followed its key to another machine would ask for a subject no live credential
-- has and every trade of it from then on would be refused with nothing naming the cure.
--
-- Every vocabulary this table holds is closed here rather than in Go alone, because what reads
-- these rows is a route that lists a person's machines to that person: the login is lowercase, as
-- every login the broker compares is; the thumbprint is the 43 base64url characters an RFC 7638
-- SHA-256 thumbprint is and nothing else, so no proof's own text can be written here; and the
-- machine's account is the address a Google identity token carries, in lowercase, as
-- BROKER_VM_SERVICES lists it. A revoked row's time is never before the row was made.
--
-- Nothing here expires, and nothing sweeps it: a sign-in ends when its person revokes it, which
-- is the whole of what R4 asks for, and a row holds no secret -- a thumbprint is public, being
-- what a DPoP proof's own key hashes to.
create table person_signins (
  id uuid primary key default gen_random_uuid(),
  person text not null check (person <> '' and person = lower(person)),
  thumbprint text not null check (thumbprint ~ '^[A-Za-z0-9_-]{43}$'),
  machine_account text not null check (machine_account <> '' and machine_account = lower(machine_account)),
  created_at timestamptz not null default now(),
  last_used_at timestamptz not null default now(),
  revoked_at timestamptz,
  check (revoked_at is null or revoked_at >= created_at)
);
-- One sign-in per person and box key, live or revoked, which is what the exchange's upsert
-- writes through: a second trade of the pair moves the row it already has, and a revoked row's
-- conflict is what refuses the trade rather than a read of its own, so a revoke committing
-- between that read and the insert cannot leave a second live row behind.
create unique index person_signins_key on person_signins (person, thumbprint);
-- The person's own list, newest first, which is what the route Dispatch relays reads.
create index person_signins_person on person_signins (person, created_at desc);

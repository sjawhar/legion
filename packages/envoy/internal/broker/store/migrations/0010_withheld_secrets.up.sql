-- packages/envoy/internal/broker/store/migrations/0010_withheld_secrets.up.sql
-- A session's operator who revokes a grant the session got without asking withholds each name its
-- request got automatically from that session: the name is human tier to the session from then on,
-- so its later requests for it are approval requests to the secret's owner, or to anyone for a
-- shared secret (requests.Machine.RevokeByApprover writes a row; Create, ApplyDecision and Values
-- evaluate the session with them). A row lasts as long as its session; nothing ends one, and an
-- ended session asks for nothing.
create table withheld_secrets (
  enrollment_id uuid not null references enrollments(id),
  name text not null,
  created_at timestamptz not null default now(),
  primary key (enrollment_id, name)
);

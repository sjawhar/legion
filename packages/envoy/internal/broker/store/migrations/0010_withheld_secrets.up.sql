-- packages/envoy/internal/broker/store/migrations/0010_withheld_secrets.up.sql
-- A person who revokes a grant ends what that session got without asking: each name the grant's
-- request was granted automatically is withheld from the session, whose later requests for it are
-- approval requests to the secret's owner, or to anyone for a shared secret
-- (requests.Machine.RevokeByApprover writes a row, Create reads them). A row lasts as long as its
-- session; nothing ends one, and an ended session asks for nothing.
create table withheld_secrets (
  enrollment_id uuid not null references enrollments(id),
  name text not null,
  -- The revoked grant that withheld the name.
  grant_id uuid not null references grants(id),
  created_at timestamptz not null default now(),
  primary key (enrollment_id, name)
);

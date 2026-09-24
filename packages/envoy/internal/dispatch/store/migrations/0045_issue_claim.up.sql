-- The session (or human) currently working an issue, so two agents never take the same work.
-- claimed_by is the claiming actor in the same shape every other actor column holds (kind, id,
-- origin, owner, service); claimed_at is when the claim was taken. A claim is set and cleared
-- as one fact, so the two columns are always both present or both null. The partial index
-- serves the "who is working what" reads: an unclaimed filter and a session's own claims.
alter table issues
    add column claimed_by jsonb,
    add column claimed_at timestamptz,
    add constraint issues_claim_complete check ((claimed_by is null) = (claimed_at is null));
create index issues_claimed on issues ((claimed_by ->> 'id')) where claimed_by is not null;

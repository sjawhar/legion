-- INFRA-264: one row per RFC 9449 DPoP proof Dispatch has accepted, the replay record the
-- library's own resource check writes through rfc9449.DPoPReplayStorage (store.DPoPProofs). A
-- caller presenting an access token the secrets broker issued signs a fresh proof of the key that
-- token is bound to for every request (internal/brokertoken); a proof presented a second time is a
-- replay, and refusing it is the whole of what this table does.
--
-- jti and htu are the proof's own claims, and together they are the context RFC 9449 Section 4.2
-- requires a 'jti' to be unique in: a conforming client may reuse a 'jti' against another target
-- URI, so a key on jti alone would refuse a request that is no replay
-- (handler/rfc9449/storage.go's DPoPReplayStorage). The htu stored is the library's normalized
-- form, so two spellings of one target URI cannot take two replay slots. expires_at is the instant
-- the proof stops being acceptable - its 'iat' plus the proof lifespan and the clock skew, which
-- the library hands the store - plus a margin for the skew between the clock that dated it and
-- this database's, so a record outlives every presentation its proof could still be accepted on.
-- The insert takes over a record of the same key whose expires_at has passed, which is what lets
-- one statement both check and set, as the broker's own does
-- (internal/broker/oauthstore/store.go, CheckAndSetDPoPProofUsed).
--
-- dpop_proofs_expires_at is the sweep's index (store.DPoPProofs.Sweep, run by the maintenance pass
-- beside the idempotency-key sweep), so the table holds the proofs of the last few minutes rather
-- than every proof ever presented.
--
-- A new table: nothing existing is read, locked or rewritten here, and the release before this one
-- neither reads nor writes it, which is what a rolling update requires of a migration
-- (cmd/dispatch/README.md, "Database migrations").
create table dpop_proofs (
  jti text not null,
  htu text not null,
  expires_at timestamptz not null,
  primary key (jti, htu)
);

create index dpop_proofs_expires_at on dpop_proofs (expires_at);

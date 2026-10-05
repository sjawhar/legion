-- 0078_embeddings_rate_limit.up.sql
-- A token-bucket budget shared by every process that calls Bedrock for
-- background embedding work (the server's own write-time poller, and a separately run
-- `envoy-dispatch backfill-embeddings` - two independent OS processes in production, each with
-- its own in-memory embed.RateLimitedEmbedder unaware of the other's traffic), so one ceiling
-- governs their combined token rate regardless of how many processes are running. In-memory
-- pacing alone left live search exposed: Bedrock throttles the whole account, not a specific
-- caller, so an unconstrained background process could still drive the account's shared quota
-- into throttling that catches a live search's own unpaced query embedding too.
--
-- One singleton row (id enforced true by the check, so a second insert never succeeds): classic
-- token-bucket bookkeeping done entirely in the update statement that reserves tokens
-- (embedqueue.reserveTokens) - refill continuously at the caller's own ceiling/60 tokens per
-- second, capped at that ceiling, then subtract the reservation, which may go negative (debt);
-- the caller waits out its own debt before its real Bedrock call, so an overdraw costs the
-- overdrawing caller wait time, never a burst the next caller also has to absorb.
create table embeddings_rate_limit (
  id boolean primary key default true check (id),
  tokens_available double precision not null,
  last_refill_at timestamptz not null default now()
);

-- Seeded at the ceiling itself (embedqueue.tokenRateCeiling, 200,000), not 0: a deploy's first
-- reservation should pace itself against real load from that point on, not wait out a debt
-- nothing ever actually drew down just because the row was new.
insert into embeddings_rate_limit (tokens_available) values (200000);

-- packages/envoy/internal/broker/store/migrations/0024_broker_outage_rule.up.sql
-- One definition of the outage rule: had no broker served, at a given time, for longer than the
-- threshold the broker that served last recorded? Migration 0019 wrote that rule inside
-- enrollment_lease_live, and the heartbeat (enroll.ServingClock.Beat) wrote it twice more in SQL
-- of its own. Three copies of one rule can disagree: a beat that gives a gap back on a rule the
-- readers do not share either moves leases no read ever held live, or leaves leases held live
-- that no beat ever gives back. Here the rule is written once, and both callers read it.
--
-- broker_outage_since answers when a broker last served, while no broker has served since for
-- longer than that broker's threshold, and null otherwise: null while a broker is serving (the
-- latest row's gap is within its threshold) and null while the table is empty, which is how
-- things stand until a broker beats once. It answers that instant rather than a yes or no
-- because the instant is what both callers need: one read of broker_serving then answers both
-- halves of enrollment_lease_live, and gives the beat the last_serving_at and the gap it gives
-- back. A yes or no would make enrollment_lease_live read the latest row twice for every row it
-- is evaluated against, and that function is not inlined: it runs once per row at the nine places
-- a read asks whether an enrollment is live. It reads the latest row through broker_serving_latest,
-- the index 0019 created, and takes no lock, so no read of a lease waits on a beat.
--
-- enrollment_lease_live keeps its name, its one timestamptz argument and the answer 0019 gave: a
-- lease is live while it has not run out, and also when it ran out while no broker was serving,
-- if it was still running at the beat that gap started from. A roll runs pods of the build before
-- this migration beside pods of the build after it, and every one of those pods calls
-- enrollment_lease_live(timestamptz) by that name and that signature, compiled into it.
create or replace function broker_outage_since(at timestamptz) returns timestamptz
  language sql stable as $$
    select latest.last_serving_at from (
      select last_serving_at, not_serving_after from broker_serving
      order by last_serving_at desc limit 1) latest
    where at - latest.last_serving_at > latest.not_serving_after $$;

create or replace function enrollment_lease_live(lease timestamptz) returns boolean
  language sql stable as $$
    select lease > now() or coalesce(lease > broker_outage_since(now()), false) $$;

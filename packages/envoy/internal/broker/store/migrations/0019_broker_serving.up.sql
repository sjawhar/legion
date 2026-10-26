-- packages/envoy/internal/broker/store/migrations/0019_broker_serving.up.sql
-- When a broker last served, so that time no broker was serving is not counted against a lease.
-- A session renews its lease through a broker; while none is serving (every replica down, or the
-- database unreachable from all of them) no lease can be renewed, and without this the first sweep
-- after the outage would end every enrollment whose lease ran out meanwhile, locking out each pod
-- until it is relaunched.
--
-- broker_serving holds one row per broker process, keyed by an id the process draws at start.
-- Every serving broker beats on its own row (enroll.ServingClock): last_serving_at moves to the
-- beat's time and never backward, and not_serving_after records how long a gap after its beat the
-- process tolerates before it counts as no broker serving (three sweeps). The latest row, the one
-- with the greatest last_serving_at, is when any broker last served. A beat that finds the latest
-- row further back than its threshold gives that gap back to every unended lease that was still
-- running when the latest row was written, then writes its own row. A beat locks only its own row,
-- so a beat held up on its row (a lock someone else holds) fails on its own deadline while every
-- other broker goes on beating. A beat deletes rows more than an hour behind the latest, so a
-- process that is gone leaves its row for an hour at most. The table starts empty, so until a
-- broker beats once nothing differs from before.
--
-- enrollment_lease_live is the one liveness test for a lease, used wherever a read asks whether an
-- enrollment is live (Lookup, Renew, the sweep, a grant's values). A lease is live while it has not
-- run out, and also when it ran out while no broker was serving, before any broker has given the
-- time back: the latest row says no broker has beaten for longer than its threshold and the lease
-- was still running at that beat. An empty table is exactly lease > now(). It reads the rows
-- without a lock, so it never waits on a beat. It names broker_serving without a schema, resolved
-- by the caller's search_path.
create table broker_serving (
  process text primary key,
  last_serving_at timestamptz not null,
  not_serving_after interval not null
);
create or replace function enrollment_lease_live(lease timestamptz) returns boolean
  language sql stable
  as $$
    select lease > now() or exists (
      select from (
        select last_serving_at, not_serving_after from broker_serving order by last_serving_at desc limit 1
      ) latest
      where now() - latest.last_serving_at > latest.not_serving_after
        and lease > latest.last_serving_at)
  $$;

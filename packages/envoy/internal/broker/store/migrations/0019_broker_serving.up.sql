-- packages/envoy/internal/broker/store/migrations/0019_broker_serving.up.sql
-- When a broker last served, so that time no broker was serving is not counted against a lease.
-- A session renews its lease through a broker; while none is serving (every replica down, or the
-- database unreachable from all of them) no lease can be renewed, and without this the first sweep
-- after the outage would end every enrollment whose lease ran out meanwhile, locking out each pod
-- until it is relaunched.
--
-- broker_serving holds one row (its key can only be true). Every serving broker beats on it
-- (enroll.ServingClock): last_serving_at moves to the beat's time and never backward, and
-- not_serving_after records how long a gap between beats the beating process tolerates before it
-- counts as no broker serving (three sweeps). A beat that finds a longer gap gives the gap back to
-- every unended lease that was still running when the last beat landed, then moves the clock.
-- The row starts with this migration's now() and no threshold, so until a broker that knows the
-- row has beaten once, nothing differs from before.
--
-- enrollment_lease_live is the one liveness test for a lease, used wherever a read asks whether an
-- enrollment is live (Lookup, Renew, the sweep, a grant's values). A lease is live while it has not
-- run out, and also when it ran out while no broker was serving, before any broker has given the
-- time back: the row says no broker has beaten for longer than its threshold and the lease was
-- still running at the last beat. A missing row or a null threshold is exactly lease > now(). It
-- reads the row without a lock, and only the beat takes one, so it never waits on the beat. It
-- names broker_serving without a schema, resolved by the caller's search_path.
create table broker_serving (
  id boolean primary key default true check (id),
  last_serving_at timestamptz not null,
  not_serving_after interval
);
insert into broker_serving (last_serving_at) values (now()) on conflict do nothing;
create or replace function enrollment_lease_live(lease timestamptz) returns boolean
  language sql stable
  as $$
    select lease > now() or coalesce((
      select not_serving_after is not null
        and now() - last_serving_at > not_serving_after
        and lease > last_serving_at
      from broker_serving), false)
  $$;

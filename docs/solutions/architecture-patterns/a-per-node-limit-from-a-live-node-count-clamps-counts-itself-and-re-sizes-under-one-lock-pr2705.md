---
title: "A per-node limit from a live node count clamps, counts itself, and re-sizes under one lock"
category: architecture-patterns
tags:
  - rate-limit
  - replicas
  - fleet
date: 2026-11-04
status: active
module: packages/envoy/internal/broker
related_issues:
  - "sjawhar/legion#2705"
---

# A per-node limit from a live node count clamps, counts itself, and re-sizes under one lock

Extends docs/solutions/architecture-patterns/taking-a-single-process-service-to-several-the-four-audits-INFRA-236.md.

That page gives the sizing arithmetic — the `N²` store-read rate, the burst that is not divided,
and "make `N` an input, or log the `N` you assumed". This is what the live input then has to do.

**Deriving the count**

- **Clamp below at the figure the old constant assumed.** A single development process, a pod
  before its first heartbeat and a fleet whose peers' heartbeats are failing then keep today's
  sizing instead of claiming the whole fleet's budget. Every unknown resolves to the tighter
  limit, never the looser one.
- **Count in the same transaction as your own heartbeat write, after writing your row.** The
  count then includes this process and can never be zero, and no separate read can observe a
  moment the fleet does not have.
- **Hand the count over only after that transaction commits.** A heartbeat that fails hands over
  nothing and the last count stands.
- **Name the count above which the sizing stops holding, and log a warning at it** rather than
  letting the arithmetic run off quietly. A rolling deploy puts the fleet one above its steady
  count for the length of the roll, so that warning fires routinely: say so where operators read
  it, or they will chase it.
- **Write the derived figures in one log line per change**, which the idempotent setter below
  gives you for free.

**Re-sizing the limiter**

- **Keep the tokens each bucket holds.** A rate change is neither a refill nor a drain: a burst is
  one caller's momentary allowance, and the rate it refills at does not change what it has now.
- **Move every rate that belongs to one sizing under the single lock the reader holds**, so no
  refusal can quote a rate no bucket runs at, and no caller is judged by one sizing's per-caller
  rate and another's shared rate.
- **Return at once when the derived sizing is unchanged**, so a setter called every heartbeat logs
  once per change rather than once per beat.

## Evidence

`sjawhar/legion#2705` (dispatch://INFRA-454), the secrets broker's reread rate.

`api.MinReplicas = 2` is the clamp (`packages/envoy/internal/broker/api/limits.go`), with the
reason in its comment: "a lone development broker, a pod before its first beat and a fleet whose
peers' beats are failing all stay at that sizing rather than taking the whole fleet's budget for
themselves". `RereadOverallFor` applies it as `n := max(replicas, MinReplicas)`, and
`MaxSupportedReplicas = 4` names the ceiling `RereadRate.SetReplicas` warns above.

`enroll.ServingClock.Beat` (`internal/broker/enroll/serving.go`) counts the fleet with
`select count(*) from broker_serving where clock_timestamp() - last_serving_at <= not_serving_after`
inside the beat's own transaction, after this process's row is written — "This process's row is
one of them, so the count is never zero, and a beat that fails hands over nothing" — and calls
`OnReplicas` only after `tx.Commit`.

`RereadRate.SetReplicas` clamps, returns on an unchanged count, then calls
`ratelimit.Keyed.SetRatesAt`, whose comment carries both halves of the re-size: "Every bucket k
holds keeps the tokens it has (`rate.Limiter.SetLimitAt`) … Both rates move under k's one lock,
because `AllowBothAt` is the only reader of either limit and holds that lock: a reread is answered
by the two rates of one sizing rather than by one rate of each, and a refusal can never carry a
rate no bucket has."

The idempotence is load-bearing beyond tidiness: a review round found the operations guide
claiming the above-ceiling warning logs "until the roll is done", which is false precisely because
the setter logs only on a change.

Proven at two, three and four real broker processes on one Postgres
(`cmd/broker/multiprocess_test.go`, `TestABrokerFleetDerivesItsRereadRate`): each pod logged its
own rate (one every 1 s, 2.25 s, 4 s), and the refusal fell back from `Retry-After: 3` to
`Retry-After: 1` once the third pod stopped.

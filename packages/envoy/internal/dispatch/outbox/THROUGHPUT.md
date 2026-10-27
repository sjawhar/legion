# Outbox throughput readings

Readings taken 2026-10-26/27 on a shared development machine under load from other test runs.
Treat the numbers as order-of-magnitude figures.

## Method

`TestOutboxThroughputUnderRemoteLatency` (`throughput_test.go`):

- A real Postgres (pgvector pg16) and a real JetStream server.
- The outbox's bus client reaches NATS through a TCP proxy that holds each chunk for half the
  round trip, in order (`testnats.StartProxy` with `testnats.ProxyLatency`), as a remote NATS hub
  does.
- Load: 200 events across 20 issues, each routed to `role:reviewer`, whose holder is a
  self-subscribed session that answers every forward with its receipt. Half are human replies to a
  session's message, which take an author lookup and a third destination. That is 500
  destinations in all: 200 issue topics, 200 role forwards to the holder and 100 session topics.
  Before the role forward (INFRA-590, `role.go`) the 200 role routes were core publishes to
  `notifications.role.reviewer`, confirmed by a flush.
- The "busy pool" case keeps a full shared pool busy. The table's first two rows ran 32
  goroutines of `select pg_sleep(0.01)` back to back on the 16-connection shared pool production
  then had. Since the shared pool holds 64, the test fills a shared pool of 8 of its own with 16
  such goroutines (`busySharedPool`), so it does not hold 64 server connections while other
  packages' tests share the server; on 2026-10-27 that read 1,050-1,260 ev/s at 50 ms and 517-566
  at 150 ms for the one-window design.
- The single-issue burst: 100 events on one issue, every one of them bound for the same three
  destinations.
- The rate is (events marked published) / (time from start until none is left).

## Results

| Design | 50 ms idle | 50 ms busy | 150 ms idle | 150 ms busy | 50 ms burst | 150 ms burst |
|---|---|---|---|---|---|---|
| Old (`origin/main` at 1357b95c2, one synchronous publish and one write per destination) | 2.7 ev/s (5.3 run alone) | 3.0 ev/s | 1.7 ev/s | 1.9 ev/s | | |
| One window per batch, every send before any answer, one outcome write, outbox pool of its own | 1,100-1,340 ev/s | 1,100-1,230 ev/s | 540-570 ev/s | 480-540 ev/s | | |
| The same one-window design, the INFRA-590 harness commit `bed0fdc4` (`main` at `c3d09eec` plus the burst cases), 2 cores, 2026-10-27 | 749.1 ev/s | 219.0 ev/s | 443.7 ev/s | 357.9 ev/s | 725.9 ev/s (100 in 138 ms) | 251.4 ev/s (100 in 398 ms) |
| Chains: each issue's sends to each destination one at a time, issues and destinations side by side (INFRA-590, two runs on 2026-10-27) | 321-323 ev/s | 169-277 ev/s | 122-124 ev/s | 113-117 ev/s | 18.6 ev/s | 6.5 ev/s |
| Chains, role routes forwarded to the holder with a receipt (INFRA-590, two runs on 2026-10-27) | 196-199 ev/s | 171-188 ev/s | 73-74 ev/s | 71-72 ev/s | 17.3-17.6 ev/s | 6.2 ev/s |
| The same, the INFRA-590 head `891e854b`, same machine as the harness-commit row | 175.5 ev/s | 183.9 ev/s | 71.3 ev/s | 70.5 ev/s | 16.7 ev/s (100 in 5.99 s) | 6.2 ev/s (100 in 16.2 s) |

The test requires 25 ev/s of the backlog cases and holds the burst cases to no rate. The old
design fails it in every case. Chains cost one round trip per event per destination of one issue:
a batch of the backlog holds five events of each issue, so it takes five round trips, and a burst
on one issue takes one round trip per event, which is the price of each destination taking an
issue's events in order whatever fails (`planChains`). With role routes forwarded to the holder
and answered by its receipt, the backlog cases took about 1.6 times as long as with role routes
published to the role's topic, and the burst cases about as long.

## Two real processes at the production step and ten times it

`TestTheOutboxLoadSteps` (`cmd/dispatch/outbox_load_steps_test.go`, build tag `multiprocess`, run
only with `DISPATCH_OUTBOX_LOAD_STEPS=1`) is black box: real `envoy-dispatch` processes on one
Postgres and one NATS (no added latency), comments on 40 issues written through the processes'
HTTP API (alternating between them with two processes), one issue routed to a role whose holder
answers forwards with its receipt, and a test session that counts each event once at its first
arrival. The publish rate is distinct events received per second while writing; the latency runs
from the event's commit (`IssuedAt`) to that arrival. The burst writes 100 comments on one issue
through one process. Same file, same 2-core machine, 2026-10-27: "before" at the harness commit
`bed0fdc4` (`main` at `c3d09eec` plus this test), "after" at `891e854b`.

| Run | Processes | Before: published/s | Before: p50 / p99 ms | After: published/s | After: p50 / p99 ms |
|---|---|---|---|---|---|
| 4 events/s for 60 s | 1 | 4.00 | 9.2 / 19.7 | 4.00 | 9.2 / 19.9 |
| 40 events/s for 60 s | 1 | 40.00 | 8.9 / 1,063.3 | 39.99 | 8.4 / 530.4 |
| 4 events/s for 60 s | 2 | 4.00 | 10.8 / 19.7 | 4.00 | 9.8 / 34.7 |
| 40 events/s for 60 s | 2 | 39.99 | 12.4 / 1,271.3 | 39.99 | 10.6 / 1,054.4 |
| Burst of 100 on one issue, drain | 1 | 981 ms (p50 9.7, p99 70.4) | | 863 ms (p50 16.7, p99 29.8) | |
| Burst of 100 on one issue, drain | 2 | 728 ms (p50 10.1, p99 17.4) | | 1,462 ms (p50 17.7, p99 48.7) | |

Every step delivered every written event, the role holder's included, before and after. The
burst's drain time includes about one second of writing one comment after another. With NATS on
the same host a chain's round trip is about a millisecond, so ordering per destination costs the
steady steps nothing; the remote-latency rows above are where it costs.

## Order and crash safety

| Test | Old design | One window per batch | Chains |
|---|---|---|---|
| `TestABacklogPublishedAcrossANATSOutageKeepsItsOrder` (NATS down before the scan, then back) | Fails: an issue's subject held `[3 5 7 … 19 1]`, because event 1 was backed off | Passes: in id order, 0.5-0.7 s after NATS came back | Passes: each issue's events in id order on each destination |
| `TestABatchCutOffInFlightKeepsItsOrder` (connection cut with acknowledgements in flight at 150 ms round trip) | Fails on the first pipelined build: 100 events backed off, then reordered | Passes: every event released, no attempt counted, stream in id order with no duplicates | Passes: every event released, no attempt counted, each destination in id order, no duplicates |
| `TestAnOutboxThatDiesBetweenAckAndMarkPublishesAgainWithoutDuplicates` | Passes | Passes: republished once the claim lapses, the stream's MsgId drops every repeat | Passes: republished once the dead holder's hold lapses, the stream's MsgId drops every repeat |
| `TestABackedOffEventHoldsBackOnlyItsDestination` (one session's subject fails for an event; or the session holds the issue's role and withholds its receipt for the event) | n/a | Fails: the session takes the later events first, `[2 3 1]` | Passes: the topic family takes the later events at once, the session all three in order; the role variant fails before the role forward, since the holder then receives nothing from the outbox |
| `TestAnIssuesEventsGoToEachDestinationOneAtATime` | n/a | n/a | Passes: nothing is marked or backed off until every answer of the batch is in |
| `TestAnOutboxStoppedMidBatchMarksTheAnsweredAndReleasesTheRest` | n/a | Passes; fails if the outcome write uses the cancelled context | Passes |

## What order consumers rely on

- The Legion daemon applies an issue's facts by their per-issue `seq` and drops one at or below the
  last it applied (`daemon/internal/workflow/engine.go`, `admit/admit.go`). So on the success
  path each issue's events must reach its subject in id order. A late older event is lost.
- The Envoy listener dedupes by `dedupe_key` and relies on no order (`cmd/listener/delivery.go`).
- The SSE stream reads Postgres, not NATS.

## Known limits, not addressed here

- A failed outcome write resends the whole batch. JetStream drops the repeats by MsgId, a role
  forward's included, and a role's holder drops a repeat forward by its dedupe key.
- A session that takes an issue's events on two destinations (its topic family and its own
  subject) can see a later event's first copy before an earlier one's when the stream fails one
  subject and not the other (`planChains`).

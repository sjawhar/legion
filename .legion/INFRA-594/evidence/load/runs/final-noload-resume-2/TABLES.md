# Load test tables

One row per step. Latencies are ms over the measured window; cores are CPU-seconds per second.

## Events, delivery and requests

| step | start (UTC) | sessions | events/s target / offered / committed | write errors non-2xx / transport | delivery ms from arrival p50 / p99 | delivery ms from commit p50 / p99 | issue-topic receipts got / expected | req/s target / sent | req 2xx | req ms p50 / p99 | requests dropped at the in-flight cap |
|---|---|---|---|---|---|---|---|---|---|---|---|
| step-10x (listener exited) | 2026-10-28 19:54:17 | 1,000 | 40.00 / 39.69 / 0.71 | 0 / 11,702 | 18,591 / 30,357 | 95 / 864 | 1,060 / 1,060 | 0 | - | - | 0 |

(<process> exited): that stack process exited during the step (summary.json `stack_exits`), so the step failed and its row, in every table here, does not measure the load.

## Write latency by kind

Write ms p50 / p99 over 2xx writes, then committed / offered.

| step | comment ms p50 / p99 (committed / offered) | anchored ms p50 / p99 (committed / offered) | reply ms p50 / p99 (committed / offered) | edit ms p50 / p99 (committed / offered) | upload ms p50 / p99 (committed / offered) |
|---|---|---|---|---|---|
| step-10x (listener exited) | - (0 / 383) | - (0 / 3,848) | - (0 / 4,246) | 23,082 / 29,700 (149 / 2,636) | 16,185 / 24,178 (63 / 794) |

## Resources

| step | Dispatch cores mean / peak 5 s | Postgres cores | listener cores | NATS cores | loadgen cores | proxy cores | box busy | box busy before | Dispatch RSS max MiB | heap in use MiB p50 / max | goroutines p50 / max | goroutines waiting for a pool connection p50 / max | Postgres connections max | connections waiting on a lock p50 | Dispatch CPU-s per operation |
|---|---|---|---|---|---|---|---|---|---|---|---|---|---|---|---|
| step-10x (listener exited) | 0.19 / 1.02 | 0.77 | - | 0.037 | 0.07 | 0.06 | 60% | 2% | 2,245 | 1,397 / 1,776 | 3,376 / 3,537 | 1,428 / 1,584 | 79 | 2 | 0.2636 |

## Session delivery, listener and NATS

| step | session delivery asked | sessions on consumer / core | delivery ms from publish p50 / p99 | delivery ms from arrival p50 / p99 | issue-topic receipts lost | relay frames / duplicates / redelivered | listener cores first / last third | listener RSS MiB first / last third (max) | NATS cores first / last third | NATS RSS MiB first / last third (max) | JetStream consumers max | pull requests/s all / session | JetStream API requests/s |
|---|---|---|---|---|---|---|---|---|---|---|---|---|---|
| step-10x (listener exited) | consumer | 1000 / 0 | 7 / 86 | 18,591 / 30,357 | 0 | 6556 / 0 / 0 | - | - | 0.052 / 0.026 | 150 / 187 (190) | 1,004 | 29.9 / 29.9 | 19.1 |

Under consumer delivery a relay frame is a session's receipt, and the delivery latency from publish is its receipt time minus the stream's store time of the event (the frame's `Envoy-Stored-At`); on the core path it is `-`. Lost is the expected issue-topic receipts minus those got. A step whose sessions answered core measured the core path.

## Commit-order lock

Sampled every 20 ms from 20 s into the window to its end.

| step | time held | waiters p50 / max | hold ms p50 / p90 / p99 | holds seen | what the holder was doing (share of held samples) |
|---|---|---|---|---|---|
| step-10x (listener exited) | 57% | 0 / 8 | 476 / 963 / 1,291 | 297 | idle in transaction (Client:ClientRead) 85%; active:  insert into events (issue_key, artifact_id, project_key, seq, 9%; active: select pg_advisory_xact_lock($1) 2%; idle in transaction (LWLock:LockManager) 2% |

## Delivery from sending, and SSE frames

NATS: every issue-topic receipt of a primary event. SSE: the dashboard stream frames that arrived in the window; from write response counts only the frames that carry the primary event of a committed in-window write with a 2xx status.

| step | NATS delivery ms from sent p50 / p99 | SSE frame ms from created_at p50 / p99 | SSE frame ms from write response p50 / p99 | SSE frames | SSE order violations |
|---|---|---|---|---|---|
| step-10x (listener exited) | 18,592 / 30,357 | 131,923 / 272,940 | - | 20 | 0 |

## Ungranted locks

Every ungranted heavyweight lock in pg_locks, sampled once a second from 20 s into the window to its end. Up to four locks per step, most samples with a waiter first.

| step | samples with an ungranted lock | locks waited for: type target mode, samples with a waiter, waiters p50 / max | top waiter query → first blocker query (count), per lock |
|---|---|---|---|
| step-10x (listener exited) | 66% | transactionid row_wait ShareLock 54%, 1 / 7; advisory commit_order_lock ExclusiveLock 33%, 0 / 7; tuple issues ExclusiveLock 2%, 0 / 1 | select status, closed_at is null from issues where key = $1 for no key update → insert into comments (id, issue_key, artifact_id, author, body, anchor, reply_t (38); select pg_advisory_xact_lock($1) → select pg_advisory_xact_lock($1) (68); select status, closed_at is null from issues where key = $1 for no key update → select status, closed_at is null from issues where key = $1 for no key update (5) |

## Transaction ids

| step | transaction ids per s |
|---|---|
| step-10x (listener exited) | 25.0 |

## Requests by route

p50 / p99 ms (failures, non-2xx or transport, / count).

| route | step-10x (listener exited) |
|---|---|

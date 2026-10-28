# Load test tables

One row per step. Latencies are ms over the measured window; cores are CPU-seconds per second.

## Events, delivery and requests

| step | start (UTC) | sessions | events/s target / offered / committed | write errors non-2xx / transport | delivery ms from arrival p50 / p99 | delivery ms from commit p50 / p99 | issue-topic receipts got / expected | req/s target / sent | req 2xx | req ms p50 / p99 | requests dropped at the in-flight cap |
|---|---|---|---|---|---|---|---|---|---|---|---|
| step-10x (listener exited) | 2026-10-28 19:46:07 | 1,000 | 40.00 / 39.69 / 0.74 | 0 / 11,695 | 18,524 / 30,748 | 101 / 1,710 | 1,105 / 1,105 | 0 | - | - | 0 |

(<process> exited): that stack process exited during the step (summary.json `stack_exits`), so the step failed and its row, in every table here, does not measure the load.

## Write latency by kind

Write ms p50 / p99 over 2xx writes, then committed / offered.

| step | comment ms p50 / p99 (committed / offered) | anchored ms p50 / p99 (committed / offered) | reply ms p50 / p99 (committed / offered) | edit ms p50 / p99 (committed / offered) | upload ms p50 / p99 (committed / offered) |
|---|---|---|---|---|---|
| step-10x (listener exited) | - (0 / 383) | - (0 / 3,848) | - (1 / 4,246) | 24,186 / 29,884 (154 / 2,636) | 15,876 / 23,987 (66 / 794) |

## Resources

| step | Dispatch cores mean / peak 5 s | Postgres cores | listener cores | NATS cores | loadgen cores | proxy cores | box busy | box busy before | Dispatch RSS max MiB | heap in use MiB p50 / max | goroutines p50 / max | goroutines waiting for a pool connection p50 / max | Postgres connections max | connections waiting on a lock p50 | Dispatch CPU-s per operation |
|---|---|---|---|---|---|---|---|---|---|---|---|---|---|---|---|
| step-10x (listener exited) | 0.18 / 1.14 | 0.81 | - | 0.040 | 0.07 | 0.07 | 62% | 3% | 1,984 | 1,376 / 1,684 | 3,332 / 3,470 | 1,446 / 1,593 | 79 | 2 | 0.2485 |

## Session delivery, listener and NATS

| step | session delivery asked | sessions on consumer / core | delivery ms from publish p50 / p99 | delivery ms from arrival p50 / p99 | issue-topic receipts lost | relay frames / duplicates / redelivered | listener cores first / last third | listener RSS MiB first / last third (max) | NATS cores first / last third | NATS RSS MiB first / last third (max) | JetStream consumers max | pull requests/s all / session | JetStream API requests/s |
|---|---|---|---|---|---|---|---|---|---|---|---|---|---|
| step-10x (listener exited) | consumer | 1000 / 0 | 6 / 43 | 18,524 / 30,748 | 0 | 6389 / 0 / 0 | - | - | 0.050 / 0.030 | 149 / 184 (192) | 1,004 | 30.0 / 30.0 | 19.2 |

Under consumer delivery a relay frame is a session's receipt, and the delivery latency from publish is its receipt time minus the stream's store time of the event (the frame's `Envoy-Stored-At`); on the core path it is `-`. Lost is the expected issue-topic receipts minus those got. A step whose sessions answered core measured the core path.

## Commit-order lock

Sampled every 20 ms from 20 s into the window to its end.

| step | time held | waiters p50 / max | hold ms p50 / p90 / p99 | holds seen | what the holder was doing (share of held samples) |
|---|---|---|---|---|---|
| step-10x (listener exited) | 66% | 0 / 8 | 476 / 934 / 1,351 | 345 | idle in transaction (Client:ClientRead) 86%; active:  insert into events (issue_key, artifact_id, project_key, seq, 7%; active: select pg_advisory_xact_lock($1) 2%; idle in transaction 2% |

## Delivery from sending, and SSE frames

NATS: every issue-topic receipt of a primary event. SSE: the dashboard stream frames that arrived in the window; from write response counts only the frames that carry the primary event of a committed in-window write with a 2xx status.

| step | NATS delivery ms from sent p50 / p99 | SSE frame ms from created_at p50 / p99 | SSE frame ms from write response p50 / p99 | SSE frames | SSE order violations |
|---|---|---|---|---|---|
| step-10x (listener exited) | 18,524 / 30,767 | 301,409 / 301,409 | - | 3 | 0 |

## Ungranted locks

Every ungranted heavyweight lock in pg_locks, sampled once a second from 20 s into the window to its end. Up to four locks per step, most samples with a waiter first.

| step | samples with an ungranted lock | locks waited for: type target mode, samples with a waiter, waiters p50 / max | top waiter query → first blocker query (count), per lock |
|---|---|---|---|
| step-10x (listener exited) | 72% | transactionid row_wait ShareLock 59%, 1 / 6; advisory commit_order_lock ExclusiveLock 37%, 0 / 8; tuple issues ExclusiveLock 3%, 0 / 2 | select status, closed_at is null from issues where key = $1 for no key update → select coalesce(i.closed_at is null, true) from artifacts a left join issues i (59); select pg_advisory_xact_lock($1) → insert into events (issue_key, artifact_id, project_key, seq, type, actor, payl (69); select status, closed_at is null from issues where key = $1 for no key update → select status, closed_at is null from issues where key = $1 for no key update (9) |

## Transaction ids

| step | transaction ids per s |
|---|---|
| step-10x (listener exited) | 26.5 |

## Requests by route

p50 / p99 ms (failures, non-2xx or transport, / count).

| route | step-10x (listener exited) |
|---|---|

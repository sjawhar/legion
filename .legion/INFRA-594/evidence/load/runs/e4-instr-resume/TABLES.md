# Load test tables

One row per step. Latencies are ms over the measured window; cores are CPU-seconds per second.

## Events, delivery and requests

| step | start (UTC) | sessions | events/s target / offered / committed | write errors non-2xx / transport | delivery ms from arrival p50 / p99 | delivery ms from commit p50 / p99 | issue-topic receipts got / expected | req/s target / sent | req 2xx | req ms p50 / p99 | requests dropped at the in-flight cap |
|---|---|---|---|---|---|---|---|---|---|---|---|
| step-10x (listener exited) | 2026-10-28 17:38:22 | 1,000 | 40.00 / 39.69 / 0.56 | 0 / 11,743 | 18,830 / 33,043 | 119 / 14,561 | 845 / 845 | 600.0 / 596.1 | 0.3% | 29,711 / 30,161 | 0 |

(<process> exited): that stack process exited during the step (summary.json `stack_exits`), so the step failed and its row, in every table here, does not measure the load.

## Write latency by kind

Write ms p50 / p99 over 2xx writes, then committed / offered.

| step | comment ms p50 / p99 (committed / offered) | anchored ms p50 / p99 (committed / offered) | reply ms p50 / p99 (committed / offered) | edit ms p50 / p99 (committed / offered) | upload ms p50 / p99 (committed / offered) |
|---|---|---|---|---|---|
| step-10x (listener exited) | 15,446 / 15,446 (3 / 383) | 20,523 / 24,441 (6 / 3,848) | 17,714 / 24,060 (20 / 4,246) | 19,685 / 29,783 (103 / 2,636) | 13,812 / 18,285 (37 / 794) |

## Resources

| step | Dispatch cores mean / peak 5 s | Postgres cores | listener cores | NATS cores | loadgen cores | proxy cores | box busy | box busy before | Dispatch RSS max MiB | heap in use MiB p50 / max | goroutines p50 / max | goroutines waiting for a pool connection p50 / max | Postgres connections max | connections waiting on a lock p50 | Dispatch CPU-s per operation |
|---|---|---|---|---|---|---|---|---|---|---|---|---|---|---|---|
| step-10x (listener exited) | 0.28 / 0.41 | 0.17 | - | 0.040 | 0.63 | 0.05 | 63% | 4% | 1,485 | 639 / 1,090 | 37,439 / 46,853 | 18,610 / 23,310 | 80 | 25 | 0.0005 |

## Session delivery, listener and NATS

| step | session delivery asked | sessions on consumer / core | delivery ms from publish p50 / p99 | delivery ms from arrival p50 / p99 | issue-topic receipts lost | relay frames / duplicates / redelivered | listener cores first / last third | listener RSS MiB first / last third (max) | NATS cores first / last third | NATS RSS MiB first / last third (max) | JetStream consumers max | pull requests/s all / session | JetStream API requests/s |
|---|---|---|---|---|---|---|---|---|---|---|---|---|---|
| step-10x (listener exited) | consumer | 1000 / 0 | 9 / 14,387 | 18,830 / 33,043 | 0 | 6017 / 1040 / 0 | - | - | 0.042 / 0.064 | 134 / 182 (188) | 1,004 | 21.0 / 21.0 | 21.6 |

Under consumer delivery a relay frame is a session's receipt, and the delivery latency from publish is its receipt time minus the stream's store time of the event (the frame's `Envoy-Stored-At`); on the core path it is `-`. Lost is the expected issue-topic receipts minus those got. A step whose sessions answered core measured the core path.

## Commit-order lock

Sampled every 20 ms from 20 s into the window to its end.

| step | time held | waiters p50 / max | hold ms p50 / p90 / p99 | holds seen | what the holder was doing (share of held samples) |
|---|---|---|---|---|---|
| step-10x (listener exited) | 19% | 0 / 60 | 644 / 2,980 / 4,370 | 46 | idle in transaction (Client:ClientRead) 89%; idle in transaction 3%; active:  insert into events (issue_key, artifact_id, project_key, seq, 3%; active:  with last_human as materialized ( select coalesce(max(id), 0) 1% |

## Delivery from sending, and SSE frames

NATS: every issue-topic receipt of a primary event. SSE: the dashboard stream frames that arrived in the window; from write response counts only the frames that carry the primary event of a committed in-window write with a 2xx status.

| step | NATS delivery ms from sent p50 / p99 | SSE frame ms from created_at p50 / p99 | SSE frame ms from write response p50 / p99 | SSE frames | SSE order violations |
|---|---|---|---|---|---|
| step-10x (listener exited) | 19,186 / 33,524 | 33,418 / 191,675 | 177,500 / 189,822 | 516 | 0 |

## Ungranted locks

Every ungranted heavyweight lock in pg_locks, sampled once a second from 20 s into the window to its end. Up to four locks per step, most samples with a waiter first.

| step | samples with an ungranted lock | locks waited for: type target mode, samples with a waiter, waiters p50 / max | top waiter query → first blocker query (count), per lock |
|---|---|---|---|
| step-10x (listener exited) | 17% | advisory commit_order_lock ExclusiveLock 17%, 0 / 58; transactionid row_wait ShareLock 14%, 0 / 3 | select pg_advisory_xact_lock($1) → with recursive chain as ( select i.parent_key, i.labels, 0 as depth from issues (285); select status, closed_at is null from issues where key = $1 for no key update → select pg_advisory_xact_lock($1) (51) |

## Transaction ids

| step | transaction ids per s |
|---|---|
| step-10x (listener exited) | 1.0 |

## Requests by route

p50 / p99 ms (failures, non-2xx or transport, / count).

| route | step-10x (listener exited) |
|---|---|
| browser GET /api/v1/inbox | 29,742 / 30,171 (7,253/7,259) |
| browser GET /api/v1/issues/{key} | 29,722 / 30,167 (39,427/39,427) |
| browser GET /api/v1/issues/{key}/events?after=0&limit=200 | 29,676 / 30,135 (3,526/3,529) |
| browser GET /api/v1/issues?project={project} | 29,686 / 30,171 (10,570/10,577) |
| browser GET /api/v1/me/replies | 29,702 / 30,202 (5,307/5,309) |
| browser GET /api/v1/me/state | 29,710 / 30,157 (3,550/3,556) |
| cli GET /api/v1/agents | 26,124 / 30,115 (1,731/1,731) |
| cli GET /api/v1/issues/{key} | 29,715 / 30,181 (12,641/12,641) |
| cli GET /api/v1/issues/{key}/events?after=0&limit=200 | 29,716 / 30,151 (12,360/12,570) |
| cli GET /api/v1/issues/{key}/references | 29,720 / 30,140 (12,467/12,473) |
| cli GET /api/v1/references?from={key_ref} | 29,717 / 30,178 (10,645/10,653) |
| cli GET /api/v1/references?to={key_ref} | 29,730 / 30,168 (10,687/10,688) |
| other GET /api/v1/asks/open?project={project} | 29,683 / 30,152 (5,356/5,453) |
| other GET /api/v1/projects | 29,741 / 30,180 (5,243/5,352) |
| other GET /api/v1/search?q=outbox+latency | 29,710 / 30,148 (7,094/7,102) |
| pod GET /api/v1/artifacts/{artifact_id}/text | 29,717 / 30,162 (17,924/17,924) |
| pod GET /api/v1/issues/{key}/comments | 29,665 / 30,156 (5,372/5,373) |
| pod GET /api/v1/issues/{key}/comments?artifact=spec | 29,735 / 30,138 (7,100/7,224) |

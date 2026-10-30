# Load test tables

One row per step. Latencies are ms over the measured window; cores are CPU-seconds per second.

## Events, delivery and requests

| step | start (UTC) | sessions | events/s target / offered / committed | write errors non-2xx / transport | delivery ms from arrival p50 / p99 | delivery ms from commit p50 / p99 | issue-topic receipts got / expected | req/s target / sent | req 2xx | req ms p50 / p99 | requests dropped at the in-flight cap |
|---|---|---|---|---|---|---|---|---|---|---|---|
| step-10x (listener exited) | 2026-10-28 17:27:03 | 1,000 | 40.00 / 39.69 / 0.75 | 0 / 11,699 | 21,544 / 31,920 | 107 / 3,662 | 1,130 / 1,130 | 600.0 / 596.1 | 0.4% | 29,782 / 30,220 | 0 |

(<process> exited): that stack process exited during the step (summary.json `stack_exits`), so the step failed and its row, in every table here, does not measure the load.

## Write latency by kind

Write ms p50 / p99 over 2xx writes, then committed / offered.

| step | comment ms p50 / p99 (committed / offered) | anchored ms p50 / p99 (committed / offered) | reply ms p50 / p99 (committed / offered) | edit ms p50 / p99 (committed / offered) | upload ms p50 / p99 (committed / offered) |
|---|---|---|---|---|---|
| step-10x (listener exited) | 9,644 / 9,644 (2 / 383) | 24,600 / 24,600 (3 / 3,848) | 3,245 / 11,100 (9 / 4,246) | 25,238 / 29,503 (154 / 2,636) | 15,470 / 20,606 (58 / 794) |

## Resources

| step | Dispatch cores mean / peak 5 s | Postgres cores | listener cores | NATS cores | loadgen cores | proxy cores | box busy | box busy before | Dispatch RSS max MiB | heap in use MiB p50 / max | goroutines p50 / max | goroutines waiting for a pool connection p50 / max | Postgres connections max | connections waiting on a lock p50 | Dispatch CPU-s per operation |
|---|---|---|---|---|---|---|---|---|---|---|---|---|---|---|---|
| step-10x (listener exited) | 0.27 / 0.58 | 0.18 | - | 0.035 | 0.62 | 0.05 | 62% | 3% | 1,607 | 861 / 1,182 | 38,238 / 40,317 | 19,010 / 20,063 | 79 | 23 | 0.0005 |

## Session delivery, listener and NATS

| step | session delivery asked | sessions on consumer / core | delivery ms from publish p50 / p99 | delivery ms from arrival p50 / p99 | issue-topic receipts lost | relay frames / duplicates / redelivered | listener cores first / last third | listener RSS MiB first / last third (max) | NATS cores first / last third | NATS RSS MiB first / last third (max) | JetStream consumers max | pull requests/s all / session | JetStream API requests/s |
|---|---|---|---|---|---|---|---|---|---|---|---|---|---|
| step-10x (listener exited) | consumer | 1000 / 0 | 7 / 173 | 21,544 / 31,920 | 0 | 5343 / 41 / 0 | - | - | 0.033 / 0.031 | 135 / 185 (186) | 1,007 | 28.3 / 28.3 | 20.5 |

Under consumer delivery a relay frame is a session's receipt, and the delivery latency from publish is its receipt time minus the stream's store time of the event (the frame's `Envoy-Stored-At`); on the core path it is `-`. Lost is the expected issue-topic receipts minus those got. A step whose sessions answered core measured the core path.

## Commit-order lock

Sampled every 20 ms from 20 s into the window to its end.

| step | time held | waiters p50 / max | hold ms p50 / p90 / p99 | holds seen | what the holder was doing (share of held samples) |
|---|---|---|---|---|---|
| step-10x (listener exited) | 17% | 0 / 41 | 1,160 / 2,966 / 4,005 | 36 | idle in transaction (Client:ClientRead) 95%; idle in transaction 2%; active:  insert into events (issue_key, artifact_id, project_key, seq, 1%; active:  with recursive chain as ( select i.parent_key, i.labels, 0 as 1% |

## Delivery from sending, and SSE frames

NATS: every issue-topic receipt of a primary event. SSE: the dashboard stream frames that arrived in the window; from write response counts only the frames that carry the primary event of a committed in-window write with a 2xx status.

| step | NATS delivery ms from sent p50 / p99 | SSE frame ms from created_at p50 / p99 | SSE frame ms from write response p50 / p99 | SSE frames | SSE order violations |
|---|---|---|---|---|---|
| step-10x (listener exited) | 22,174 / 33,200 | 71,571 / 208,242 | 456 / 174,309 | 810 | 0 |

## Ungranted locks

Every ungranted heavyweight lock in pg_locks, sampled once a second from 20 s into the window to its end. Up to four locks per step, most samples with a waiter first.

| step | samples with an ungranted lock | locks waited for: type target mode, samples with a waiter, waiters p50 / max | top waiter query → first blocker query (count), per lock |
|---|---|---|---|
| step-10x (listener exited) | 17% | advisory commit_order_lock ExclusiveLock 17%, 0 / 41 | select pg_advisory_xact_lock($1) → select pg_advisory_xact_lock($1) (219) |

## Transaction ids

| step | transaction ids per s |
|---|---|
| step-10x (listener exited) | 0.8 |

## Requests by route

p50 / p99 ms (failures, non-2xx or transport, / count).

| route | step-10x (listener exited) |
|---|---|
| browser GET /api/v1/inbox | 29,770 / 30,193 (7,230/7,259) |
| browser GET /api/v1/issues/{key} | 29,782 / 30,211 (39,419/39,427) |
| browser GET /api/v1/issues/{key}/events?after=0&limit=200 | 29,778 / 30,207 (3,517/3,529) |
| browser GET /api/v1/issues?project={project} | 29,763 / 30,212 (10,543/10,577) |
| browser GET /api/v1/me/replies | 29,806 / 30,236 (5,293/5,309) |
| browser GET /api/v1/me/state | 29,804 / 30,175 (3,542/3,556) |
| cli GET /api/v1/agents | 29,334 / 30,121 (1,721/1,731) |
| cli GET /api/v1/issues/{key} | 29,796 / 30,227 (12,638/12,641) |
| cli GET /api/v1/issues/{key}/events?after=0&limit=200 | 29,776 / 30,218 (12,429/12,570) |
| cli GET /api/v1/issues/{key}/references | 29,779 / 30,235 (12,423/12,473) |
| cli GET /api/v1/references?from={key_ref} | 29,789 / 30,192 (10,598/10,653) |
| cli GET /api/v1/references?to={key_ref} | 29,784 / 30,250 (10,653/10,688) |
| other GET /api/v1/asks/open?project={project} | 29,793 / 30,259 (5,384/5,453) |
| other GET /api/v1/projects | 29,789 / 30,199 (5,291/5,352) |
| other GET /api/v1/search?q=outbox+latency | 29,770 / 30,228 (7,075/7,102) |
| pod GET /api/v1/artifacts/{artifact_id}/text | 29,797 / 30,237 (17,899/17,924) |
| pod GET /api/v1/issues/{key}/comments | 29,782 / 30,202 (5,350/5,373) |
| pod GET /api/v1/issues/{key}/comments?artifact=spec | 29,780 / 30,244 (7,142/7,224) |

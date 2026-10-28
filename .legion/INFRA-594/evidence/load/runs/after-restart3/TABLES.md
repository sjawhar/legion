# Load test tables

One row per step. Latencies are ms over the measured window; cores are CPU-seconds per second.

## Events, delivery and requests

| step | start (UTC) | sessions | events/s target / offered / committed | write errors non-2xx / transport | delivery ms from arrival p50 / p99 | delivery ms from commit p50 / p99 | issue-topic receipts got / expected | req/s target / sent | req 2xx | req ms p50 / p99 | requests dropped at the in-flight cap |
|---|---|---|---|---|---|---|---|---|---|---|---|
| step-10x (listener exited) | 2026-10-28 13:18:41 | 1,000 | 40.00 / 39.69 / 0.79 | 0 / 11,686 | 20,728 / 30,328 | 97 / 3,875 | 1,185 / 1,185 | 600.0 / 596.1 | 0.0% | 29,958 / 30,339 | 0 |

(<process> exited): that stack process exited during the step (summary.json `stack_exits`), so the step failed and its row, in every table here, does not measure the load.

## Write latency by kind

Write ms p50 / p99 over 2xx writes, then committed / offered.

| step | comment ms p50 / p99 (committed / offered) | anchored ms p50 / p99 (committed / offered) | reply ms p50 / p99 (committed / offered) | edit ms p50 / p99 (committed / offered) | upload ms p50 / p99 (committed / offered) |
|---|---|---|---|---|---|
| step-10x (listener exited) | - (0 / 383) | - (0 / 3,848) | - (0 / 4,246) | 26,135 / 29,686 (170 / 2,636) | 14,974 / 20,721 (67 / 794) |

## Resources

| step | Dispatch cores mean / peak 5 s | Postgres cores | listener cores | NATS cores | loadgen cores | proxy cores | box busy | box busy before | Dispatch RSS max MiB | heap in use MiB p50 / max | goroutines p50 / max | goroutines waiting for a pool connection p50 / max | Postgres connections max | connections waiting on a lock p50 | Dispatch CPU-s per operation |
|---|---|---|---|---|---|---|---|---|---|---|---|---|---|---|---|
| step-10x (listener exited) | 0.29 / 0.39 | 0.20 | - | 0.054 | 0.57 | 0.05 | 62% | 15% | 1,666 | 788 / 1,187 | 38,700 / 41,069 | 19,217 / 20,439 | 79 | 16 | 0.0005 |

## Session delivery, listener and NATS

| step | session delivery asked | sessions on consumer / core | delivery ms from publish p50 / p99 | delivery ms from arrival p50 / p99 | issue-topic receipts lost | relay frames / duplicates / redelivered | listener cores first / last third | listener RSS MiB first / last third (max) | NATS cores first / last third | NATS RSS MiB first / last third (max) | JetStream consumers max | pull requests/s all / session | JetStream API requests/s |
|---|---|---|---|---|---|---|---|---|---|---|---|---|---|
| step-10x (listener exited) | consumer | 1000 / 0 | 6 / 53 | 20,728 / 30,328 | 0 | 5434 / 0 / 0 | - | - | 0.035 / 0.060 | 142 / 169 (177) | 1,004 | 27.0 / 27.0 | 54.6 |

Under consumer delivery a relay frame is a session's receipt, and the delivery latency from publish is its receipt time minus the stream's store time of the event (the frame's `Envoy-Stored-At`); on the core path it is `-`. Lost is the expected issue-topic receipts minus those got. A step whose sessions answered core measured the core path.

## Commit-order lock

Sampled every 20 ms from 20 s into the window to its end.

| step | time held | waiters p50 / max | hold ms p50 / p90 / p99 | holds seen | what the holder was doing (share of held samples) |
|---|---|---|---|---|---|
| step-10x (listener exited) | 0% | 0 / 0 | - | 0 |  |

## Delivery from sending, and SSE frames

NATS: every issue-topic receipt of a primary event. SSE: the dashboard stream frames that arrived in the window; from write response counts only the frames that carry the primary event of a committed in-window write with a 2xx status.

| step | NATS delivery ms from sent p50 / p99 | SSE frame ms from created_at p50 / p99 | SSE frame ms from write response p50 / p99 | SSE frames | SSE order violations |
|---|---|---|---|---|---|
| step-10x (listener exited) | 20,732 / 30,328 | 203,715 / 327,440 | - | 159 | 0 |

## Ungranted locks

Every ungranted heavyweight lock in pg_locks, sampled once a second from 20 s into the window to its end. Up to four locks per step, most samples with a waiter first.

| step | samples with an ungranted lock | locks waited for: type target mode, samples with a waiter, waiters p50 / max | top waiter query → first blocker query (count), per lock |
|---|---|---|---|
| step-10x (listener exited) | 0% | - | - |

## Transaction ids

| step | transaction ids per s |
|---|---|
| step-10x (listener exited) | 0.3 |

## Requests by route

p50 / p99 ms (failures, non-2xx or transport, / count).

| route | step-10x (listener exited) |
|---|---|
| browser GET /api/v1/inbox | 29,959 / 30,414 (7,259/7,259) |
| browser GET /api/v1/issues/{key} | 29,958 / 30,343 (39,427/39,427) |
| browser GET /api/v1/issues/{key}/events?after=0&limit=200 | 29,957 / 30,223 (3,529/3,529) |
| browser GET /api/v1/issues?project={project} | 29,958 / 30,317 (10,577/10,577) |
| browser GET /api/v1/me/replies | 29,959 / 30,343 (5,309/5,309) |
| browser GET /api/v1/me/state | 29,958 / 30,292 (3,556/3,556) |
| cli GET /api/v1/agents | 29,942 / 30,154 (1,731/1,731) |
| cli GET /api/v1/issues/{key} | 29,958 / 30,388 (12,641/12,641) |
| cli GET /api/v1/issues/{key}/events?after=0&limit=200 | 29,959 / 30,373 (12,565/12,570) |
| cli GET /api/v1/issues/{key}/references | 29,957 / 30,321 (12,473/12,473) |
| cli GET /api/v1/references?from={key_ref} | 29,959 / 30,317 (10,653/10,653) |
| cli GET /api/v1/references?to={key_ref} | 29,959 / 30,413 (10,688/10,688) |
| other GET /api/v1/asks/open?project={project} | 29,958 / 30,256 (5,452/5,453) |
| other GET /api/v1/projects | 29,960 / 30,315 (5,347/5,352) |
| other GET /api/v1/search?q=outbox+latency | 29,960 / 30,289 (7,102/7,102) |
| pod GET /api/v1/artifacts/{artifact_id}/text | 29,959 / 30,360 (17,924/17,924) |
| pod GET /api/v1/issues/{key}/comments | 29,956 / 30,369 (5,373/5,373) |
| pod GET /api/v1/issues/{key}/comments?artifact=spec | 29,956 / 30,327 (7,221/7,224) |

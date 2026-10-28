# Load test tables

One row per step. Latencies are ms over the measured window; cores are CPU-seconds per second.

## Events, delivery and requests

| step | start (UTC) | sessions | events/s target / offered / committed | write errors non-2xx / transport | delivery ms from arrival p50 / p99 | delivery ms from commit p50 / p99 | issue-topic receipts got / expected | req/s target / sent | req 2xx | req ms p50 / p99 | requests dropped at the in-flight cap |
|---|---|---|---|---|---|---|---|---|---|---|---|
| step-10x (listener exited) | 2026-10-28 16:28:03 | 1,000 | 40.00 / 39.69 / 0.82 | 0 / 11,677 | 21,396 / 31,686 | 97 / 3,537 | 1,230 / 1,230 | 600.0 / 596.1 | 0.0% | 29,958 / 30,420 | 0 |

(<process> exited): that stack process exited during the step (summary.json `stack_exits`), so the step failed and its row, in every table here, does not measure the load.

## Write latency by kind

Write ms p50 / p99 over 2xx writes, then committed / offered.

| step | comment ms p50 / p99 (committed / offered) | anchored ms p50 / p99 (committed / offered) | reply ms p50 / p99 (committed / offered) | edit ms p50 / p99 (committed / offered) | upload ms p50 / p99 (committed / offered) |
|---|---|---|---|---|---|
| step-10x (listener exited) | - (0 / 383) | - (0 / 3,848) | - (0 / 4,246) | 25,901 / 29,482 (180 / 2,636) | 14,836 / 20,449 (66 / 794) |

## Resources

| step | Dispatch cores mean / peak 5 s | Postgres cores | listener cores | NATS cores | loadgen cores | proxy cores | box busy | box busy before | Dispatch RSS max MiB | heap in use MiB p50 / max | goroutines p50 / max | goroutines waiting for a pool connection p50 / max | Postgres connections max | connections waiting on a lock p50 | Dispatch CPU-s per operation |
|---|---|---|---|---|---|---|---|---|---|---|---|---|---|---|---|
| step-10x (listener exited) (1 failed reads) | 0.28 / 0.36 | 0.20 | - | 0.037 | 0.57 | 0.06 | 62% | 2% | 1,564 | 822 / 1,031 | 38,680 / 39,676 | 19,229 / 19,749 | 79 | 2 | 0.0005 |

(<n> failed reads): the 5 s sampler's reads of pg_stat_activity and of Dispatch's goroutine and heap profiles failed n times, a failed connect included (summary.json `resources.pg_read_errors` and `resources.pprof_read_errors`); those reads enter no statistic, and a cell with no successful read shows `-`, not 0.

## Session delivery, listener and NATS

| step | session delivery asked | sessions on consumer / core | delivery ms from publish p50 / p99 | delivery ms from arrival p50 / p99 | issue-topic receipts lost | relay frames / duplicates / redelivered | listener cores first / last third | listener RSS MiB first / last third (max) | NATS cores first / last third | NATS RSS MiB first / last third (max) | JetStream consumers max | pull requests/s all / session | JetStream API requests/s |
|---|---|---|---|---|---|---|---|---|---|---|---|---|---|
| step-10x (listener exited) | consumer | 1000 / 0 | 6 / 31 | 21,396 / 31,686 | 0 | 5441 / 0 / 0 | - | - | 0.041 / 0.026 | 132 / 171 (173) | 1,004 | 30.1 / 30.1 | 30.5 |

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
| step-10x (listener exited) | 21,396 / 31,739 | 203,871 / 305,884 | - | 144 | 0 |

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
| browser GET /api/v1/inbox | 29,957 / 30,388 (7,259/7,259) |
| browser GET /api/v1/issues/{key} | 29,958 / 30,427 (39,427/39,427) |
| browser GET /api/v1/issues/{key}/events?after=0&limit=200 | 29,957 / 30,378 (3,529/3,529) |
| browser GET /api/v1/issues?project={project} | 29,958 / 30,381 (10,577/10,577) |
| browser GET /api/v1/me/replies | 29,961 / 30,419 (5,309/5,309) |
| browser GET /api/v1/me/state | 29,958 / 30,491 (3,556/3,556) |
| cli GET /api/v1/agents | 29,935 / 30,226 (1,731/1,731) |
| cli GET /api/v1/issues/{key} | 29,957 / 30,463 (12,641/12,641) |
| cli GET /api/v1/issues/{key}/events?after=0&limit=200 | 29,958 / 30,408 (12,563/12,570) |
| cli GET /api/v1/issues/{key}/references | 29,958 / 30,434 (12,473/12,473) |
| cli GET /api/v1/references?from={key_ref} | 29,959 / 30,454 (10,653/10,653) |
| cli GET /api/v1/references?to={key_ref} | 29,959 / 30,404 (10,688/10,688) |
| other GET /api/v1/asks/open?project={project} | 29,960 / 30,428 (5,448/5,453) |
| other GET /api/v1/projects | 29,957 / 30,362 (5,347/5,352) |
| other GET /api/v1/search?q=outbox+latency | 29,958 / 30,387 (7,102/7,102) |
| pod GET /api/v1/artifacts/{artifact_id}/text | 29,958 / 30,451 (17,924/17,924) |
| pod GET /api/v1/issues/{key}/comments | 29,958 / 30,417 (5,373/5,373) |
| pod GET /api/v1/issues/{key}/comments?artifact=spec | 29,960 / 30,446 (7,217/7,224) |

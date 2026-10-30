# Load test tables

One row per step. Latencies are ms over the measured window; cores are CPU-seconds per second.

## Events, delivery and requests

| step | start (UTC) | sessions | events/s target / offered / committed | write errors non-2xx / transport | delivery ms from arrival p50 / p99 | delivery ms from commit p50 / p99 | issue-topic receipts got / expected | req/s target / sent | req 2xx | req ms p50 / p99 | requests dropped at the in-flight cap |
|---|---|---|---|---|---|---|---|---|---|---|---|
| restart-noresume-1 (listener exited) | 2026-10-29 15:51:04 | 1,000 | 40.00 / 39.69 / 0.95 | 0 / 11,624 | 16,925 / 30,080 | 68 / 1,658 | 1,430 / 1,430 | 600.0 / 596.1 | 0.0% | 29,987 / 30,044 | 0 |

(<process> exited): that stack process exited during the step (summary.json `stack_exits`), so the step failed and its row, in every table here, does not measure the load.

## Write latency by kind

Write ms p50 / p99 over 2xx writes, then committed / offered.

| step | comment ms p50 / p99 (committed / offered) | anchored ms p50 / p99 (committed / offered) | reply ms p50 / p99 (committed / offered) | edit ms p50 / p99 (committed / offered) | upload ms p50 / p99 (committed / offered) |
|---|---|---|---|---|---|
| restart-noresume-1 (listener exited) | - (0 / 383) | - (0 / 3,848) | - (0 / 4,246) | 17,714 / 29,655 (213 / 2,636) | 11,601 / 16,385 (73 / 794) |

## Resources

| step | Dispatch cores mean / peak 5 s | Postgres cores | listener cores | NATS cores | loadgen cores | proxy cores | box busy | box busy before | Dispatch RSS max MiB | heap in use MiB p50 / max | goroutines p50 / max | goroutines waiting for a pool connection p50 / max | Postgres connections max | connections waiting on a lock p50 | Dispatch CPU-s per operation |
|---|---|---|---|---|---|---|---|---|---|---|---|---|---|---|---|
| restart-noresume-1 (listener exited) | 0.48 / 0.80 | 0.58 | - | 0.041 | 0.75 | 0.14 | 68% | 2% | 2,616 | 1,519 / 1,938 | 39,069 / 39,656 | 19,322 / 19,580 | 81 | 59 | 0.0008 |

## Session delivery, listener and NATS

| step | session delivery asked | sessions on consumer / core | delivery ms from publish p50 / p99 | delivery ms from arrival p50 / p99 | issue-topic receipts lost | relay frames / duplicates / redelivered | listener cores first / last third | listener RSS MiB first / last third (max) | NATS cores first / last third | NATS RSS MiB first / last third (max) | JetStream consumers max | pull requests/s all / session | JetStream API requests/s |
|---|---|---|---|---|---|---|---|---|---|---|---|---|---|
| restart-noresume-1 (listener exited) | consumer | 1000 / 0 | 5 / 47 | 16,925 / 30,080 | 0 | 5531 / 0 / 0 | - | - | 0.040 / 0.028 | 143 / 191 (191) | 1,004 | 33.3 / 33.3 | 20.1 |

Under consumer delivery a relay frame is a session's receipt, and the delivery latency from publish is its receipt time minus the stream's store time of the event (the frame's `Envoy-Stored-At`); on the core path it is `-`. Lost is the expected issue-topic receipts minus those got. A step whose sessions answered core measured the core path.

## Commit-order lock

Sampled every 20 ms from 20 s into the window to its end.

| step | time held | waiters p50 / max | hold ms p50 / p90 / p99 | holds seen | what the holder was doing (share of held samples) |
|---|---|---|---|---|---|
| restart-noresume-1 (listener exited) | 0% | 0 / 0 | - | 0 |  |

## Delivery from sending, and live socket frames

NATS: every issue-topic receipt of a primary event. Live: the event frames of the dashboard live sockets that arrived in the window; from write response counts only the frames that carry the primary event of a committed in-window write with a 2xx status.

| step | NATS delivery ms from sent p50 / p99 | live frame ms from created_at p50 / p99 | live frame ms from write response p50 / p99 | live frames | live order violations |
|---|---|---|---|---|---|
| restart-noresume-1 (listener exited) | 16,930 / 30,115 | 65,472 / 72,479 | - | 1,300 | 0 |

## Ungranted locks

Every ungranted heavyweight lock in pg_locks, sampled once a second from 20 s into the window to its end. Up to four locks per step, most samples with a waiter first.

| step | samples with an ungranted lock | locks waited for: type target mode, samples with a waiter, waiters p50 / max | top waiter query → first blocker query (count), per lock |
|---|---|---|---|
| restart-noresume-1 (listener exited) | 0% | - | - |

## Transaction ids

| step | transaction ids per s |
|---|---|
| restart-noresume-1 (listener exited) | 1.4 |

## Requests by route

p50 / p99 ms (failures, non-2xx or transport, / count).

| route | restart-noresume-1 (listener exited) |
|---|---|
| browser GET /api/v1/inbox | 29,987 / 30,046 (7,259/7,259) |
| browser GET /api/v1/issues/{key} | 29,987 / 30,042 (39,427/39,427) |
| browser GET /api/v1/issues/{key}/events?after=0&limit=200 | 29,988 / 30,045 (3,529/3,529) |
| browser GET /api/v1/issues?project={project} | 29,987 / 30,043 (10,577/10,577) |
| browser GET /api/v1/me/replies | 29,987 / 30,049 (5,309/5,309) |
| browser GET /api/v1/me/state | 29,987 / 30,047 (3,556/3,556) |
| cli GET /api/v1/agents | 29,986 / 30,039 (1,731/1,731) |
| cli GET /api/v1/issues/{key} | 29,988 / 30,045 (12,641/12,641) |
| cli GET /api/v1/issues/{key}/events?after=0&limit=200 | 29,987 / 30,043 (12,566/12,570) |
| cli GET /api/v1/issues/{key}/references | 29,988 / 30,045 (12,473/12,473) |
| cli GET /api/v1/references?from={key_ref} | 29,987 / 30,045 (10,653/10,653) |
| cli GET /api/v1/references?to={key_ref} | 29,987 / 30,039 (10,688/10,688) |
| other GET /api/v1/asks/open?project={project} | 29,987 / 30,046 (5,451/5,453) |
| other GET /api/v1/projects | 29,987 / 30,049 (5,352/5,352) |
| other GET /api/v1/search?q=outbox+latency | 29,987 / 30,048 (7,102/7,102) |
| pod GET /api/v1/artifacts/{artifact_id}/text | 29,987 / 30,045 (17,924/17,924) |
| pod GET /api/v1/issues/{key}/comments | 29,987 / 30,049 (5,373/5,373) |
| pod GET /api/v1/issues/{key}/comments?artifact=spec | 29,987 / 30,039 (7,219/7,224) |

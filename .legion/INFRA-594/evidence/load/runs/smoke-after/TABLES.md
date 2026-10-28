# Load test tables

One row per step. Latencies are ms over the measured window; cores are CPU-seconds per second.

## Events, delivery and requests

| step | start (UTC) | sessions | events/s target / offered / committed | write errors non-2xx / transport | delivery ms from arrival p50 / p99 | delivery ms from commit p50 / p99 | issue-topic receipts got / expected | req/s target / sent | req 2xx | req ms p50 / p99 | requests dropped at the in-flight cap |
|---|---|---|---|---|---|---|---|---|---|---|---|
| step-10x | 2026-10-28 10:14:49 | 1,000 | 40.00 / 40.12 / 2.50 | 0 / 2,263 | 19,175 / 30,885 | 93 / 2,336 | 750 / 750 | 600.0 / 590.5 | 0.0% | 29,974 / 30,117 | 0 |

## Write latency by kind

Write ms p50 / p99 over 2xx writes, then committed / offered.

| step | comment ms p50 / p99 (committed / offered) | anchored ms p50 / p99 (committed / offered) | reply ms p50 / p99 (committed / offered) | edit ms p50 / p99 (committed / offered) | upload ms p50 / p99 (committed / offered) |
|---|---|---|---|---|---|
| step-10x | - (0 / 74) | - (0 / 761) | - (0 / 839) | 19,670 / 27,685 (114 / 555) | 14,256 / 21,213 (36 / 178) |

## Resources

| step | Dispatch cores mean / peak 5 s | Postgres cores | listener cores | NATS cores | loadgen cores | proxy cores | box busy | box busy before | Dispatch RSS max MiB | heap in use MiB p50 / max | goroutines p50 / max | goroutines waiting for a pool connection p50 / max | Postgres connections max | connections waiting on a lock p50 | Dispatch CPU-s per operation |
|---|---|---|---|---|---|---|---|---|---|---|---|---|---|---|---|
| step-10x | 0.29 / 0.42 | 0.29 | 0.061 | 0.032 | 0.48 | 0.06 | 61% | 1% | 1,678 | 978 / 1,044 | 38,259 / 38,760 | 19,032 / 19,277 | 79 | 18 | 0.0005 |

## Session delivery, listener and NATS

| step | session delivery asked | sessions on consumer / core | delivery ms from publish p50 / p99 | delivery ms from arrival p50 / p99 | issue-topic receipts lost | relay frames / duplicates / redelivered | listener cores first / last third | listener RSS MiB first / last third (max) | NATS cores first / last third | NATS RSS MiB first / last third (max) | JetStream consumers max | pull requests/s all / session | JetStream API requests/s |
|---|---|---|---|---|---|---|---|---|---|---|---|---|---|
| step-10x | consumer | 1000 / 0 | 7 / 23 | 19,175 / 30,885 | 0 | 1102 / 0 / 0 | 0.069 / 0.058 | 79 / 84 (92) | 0.037 / 0.031 | 139 / 142 (142) | 1,004 | 33.4 / 33.4 | 24.0 |

Under consumer delivery a relay frame is a session's receipt, and the delivery latency from publish is its receipt time minus the stream's store time of the event (the frame's `Envoy-Stored-At`); on the core path it is `-`. Lost is the expected issue-topic receipts minus those got. A step whose sessions answered core measured the core path.

## Commit-order lock

Sampled every 20 ms from 20 s into the window to its end.

| step | time held | waiters p50 / max | hold ms p50 / p90 / p99 | holds seen | what the holder was doing (share of held samples) |
|---|---|---|---|---|---|
| step-10x | 0% | 0 / 0 | - | 0 |  |

## Delivery from sending, and SSE frames

NATS: every issue-topic receipt of a primary event. SSE: the dashboard stream frames that arrived in the window; from write response counts only the frames that carry the primary event of a committed in-window write with a 2xx status.

| step | NATS delivery ms from sent p50 / p99 | SSE frame ms from created_at p50 / p99 | SSE frame ms from write response p50 / p99 | SSE frames | SSE order violations |
|---|---|---|---|---|---|
| step-10x | 19,175 / 30,889 | - | - | 0 | 0 |

## Ungranted locks

Every ungranted heavyweight lock in pg_locks, sampled once a second from 20 s into the window to its end. Up to four locks per step, most samples with a waiter first.

| step | samples with an ungranted lock | locks waited for: type target mode, samples with a waiter, waiters p50 / max | top waiter query → first blocker query (count), per lock |
|---|---|---|---|
| step-10x | 0% | - | - |

## Transaction ids

| step | transaction ids per s |
|---|---|
| step-10x | 1.1 |

## Requests by route

p50 / p99 ms (failures, non-2xx or transport, / count).

| route | step-10x |
|---|---|
| browser GET /api/v1/inbox | 29,974 / 30,059 (1,467/1,467) |
| browser GET /api/v1/issues/{key} | 29,974 / 30,119 (7,832/7,832) |
| browser GET /api/v1/issues/{key}/events?after=0&limit=200 | 29,972 / 30,115 (697/697) |
| browser GET /api/v1/issues?project={project} | 29,974 / 30,116 (2,136/2,136) |
| browser GET /api/v1/me/replies | 29,977 / 30,141 (1,044/1,044) |
| browser GET /api/v1/me/state | 29,975 / 30,128 (656/656) |
| cli GET /api/v1/agents | 29,976 / 30,024 (335/335) |
| cli GET /api/v1/issues/{key} | 29,976 / 30,116 (2,483/2,483) |
| cli GET /api/v1/issues/{key}/events?after=0&limit=200 | 29,973 / 30,088 (2,534/2,539) |
| cli GET /api/v1/issues/{key}/references | 29,975 / 30,138 (2,457/2,457) |
| cli GET /api/v1/references?from={key_ref} | 29,974 / 30,075 (2,082/2,082) |
| cli GET /api/v1/references?to={key_ref} | 29,973 / 30,076 (2,095/2,095) |
| other GET /api/v1/asks/open?project={project} | 29,974 / 30,142 (1,099/1,102) |
| other GET /api/v1/projects | 29,971 / 30,059 (1,086/1,088) |
| other GET /api/v1/search?q=outbox+latency | 29,975 / 30,143 (1,416/1,416) |
| pod GET /api/v1/artifacts/{artifact_id}/text | 29,974 / 30,092 (3,518/3,518) |
| pod GET /api/v1/issues/{key}/comments | 29,976 / 30,098 (1,049/1,049) |
| pod GET /api/v1/issues/{key}/comments?artifact=spec | 29,976 / 30,120 (1,427/1,431) |

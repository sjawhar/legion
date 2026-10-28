# Load test tables

One row per step. Latencies are ms over the measured window; cores are CPU-seconds per second.

## Events, delivery and requests

| step | start (UTC) | sessions | events/s target / offered / committed | write errors non-2xx / transport | delivery ms from arrival p50 / p99 | delivery ms from commit p50 / p99 | issue-topic receipts got / expected | req/s target / sent | req 2xx | req ms p50 / p99 | requests dropped at the in-flight cap |
|---|---|---|---|---|---|---|---|---|---|---|---|
| step-10x | 2026-10-28 12:07:34 | 1,000 | 40.00 / 39.89 / 0.53 | 0 / 23,632 | 20,880 / 30,982 | 81 / 3,395 | 1,585 / 1,585 | 600.0 / 597.9 | 0.0% | 29,981 / 30,115 | 0 |

## Write latency by kind

Write ms p50 / p99 over 2xx writes, then committed / offered.

| step | comment ms p50 / p99 (committed / offered) | anchored ms p50 / p99 (committed / offered) | reply ms p50 / p99 (committed / offered) | edit ms p50 / p99 (committed / offered) | upload ms p50 / p99 (committed / offered) |
|---|---|---|---|---|---|
| step-10x | - (0 / 707) | - (0 / 7,846) | - (0 / 8,512) | 26,199 / 29,889 (227 / 5,239) | 13,263 / 22,927 (90 / 1,629) |

## Resources

| step | Dispatch cores mean / peak 5 s | Postgres cores | listener cores | NATS cores | loadgen cores | proxy cores | box busy | box busy before | Dispatch RSS max MiB | heap in use MiB p50 / max | goroutines p50 / max | goroutines waiting for a pool connection p50 / max | Postgres connections max | connections waiting on a lock p50 | Dispatch CPU-s per operation |
|---|---|---|---|---|---|---|---|---|---|---|---|---|---|---|---|
| step-10x | 0.28 / 0.46 | 0.30 | 0.046 | 0.008 | 0.51 | 0.07 | 61% | 1% | 1,905 | 1,108 / 1,432 | 38,977 / 40,263 | 19,357 / 20,014 | 79 | 17 | 0.0005 |

## Commit-order lock

Sampled every 20 ms from 20 s into the window to its end.

| step | time held | waiters p50 / max | hold ms p50 / p90 / p99 | holds seen | what the holder was doing (share of held samples) |
|---|---|---|---|---|---|
| step-10x | 0% | 0 / 0 | - | 0 |  |

## Delivery from sending, and SSE frames

NATS: every issue-topic receipt of a primary event. SSE: the dashboard stream frames that arrived in the window; from write response counts only the frames that carry the primary event of a committed in-window write with a 2xx status.

| step | NATS delivery ms from sent p50 / p99 | SSE frame ms from created_at p50 / p99 | SSE frame ms from write response p50 / p99 | SSE frames | SSE order violations |
|---|---|---|---|---|---|
| step-10x | 20,880 / 31,003 | 352,282 / 605,406 | - | 424 | 0 |

## Ungranted locks

Every ungranted heavyweight lock in pg_locks, sampled once a second from 20 s into the window to its end. Up to four locks per step, most samples with a waiter first.

| step | samples with an ungranted lock | locks waited for: type target mode, samples with a waiter, waiters p50 / max | top waiter query → first blocker query (count), per lock |
|---|---|---|---|
| step-10x | 0% | - | - |

## Transaction ids

| step | transaction ids per s |
|---|---|
| step-10x | 0.3 |

## Requests by route

p50 / p99 ms (failures, non-2xx or transport, / count).

| route | step-10x |
|---|---|
| browser GET /api/v1/inbox | 29,981 / 30,132 (14,494/14,494) |
| browser GET /api/v1/issues/{key} | 29,981 / 30,120 (79,066/79,066) |
| browser GET /api/v1/issues/{key}/events?after=0&limit=200 | 29,980 / 30,113 (7,119/7,119) |
| browser GET /api/v1/issues?project={project} | 29,981 / 30,115 (21,385/21,385) |
| browser GET /api/v1/me/replies | 29,981 / 30,104 (10,755/10,755) |
| browser GET /api/v1/me/state | 29,980 / 30,123 (7,133/7,133) |
| cli GET /api/v1/agents | 29,980 / 30,104 (3,535/3,535) |
| cli GET /api/v1/issues/{key} | 29,980 / 30,115 (25,106/25,106) |
| cli GET /api/v1/issues/{key}/events?after=0&limit=200 | 29,981 / 30,109 (25,003/25,019) |
| cli GET /api/v1/issues/{key}/references | 29,981 / 30,115 (25,085/25,085) |
| cli GET /api/v1/references?from={key_ref} | 29,981 / 30,108 (21,461/21,461) |
| cli GET /api/v1/references?to={key_ref} | 29,981 / 30,117 (21,538/21,538) |
| other GET /api/v1/asks/open?project={project} | 29,981 / 30,128 (10,787/10,790) |
| other GET /api/v1/projects | 29,981 / 30,117 (10,829/10,833) |
| other GET /api/v1/search?q=outbox+latency | 29,981 / 30,110 (14,337/14,337) |
| pod GET /api/v1/artifacts/{artifact_id}/text | 29,980 / 30,116 (35,892/35,892) |
| pod GET /api/v1/issues/{key}/comments | 29,980 / 30,108 (10,705/10,705) |
| pod GET /api/v1/issues/{key}/comments?artifact=spec | 29,980 / 30,112 (14,444/14,461) |

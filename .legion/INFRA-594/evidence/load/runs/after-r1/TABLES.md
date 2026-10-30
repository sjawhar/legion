# Load test tables

One row per step. Latencies are ms over the measured window; cores are CPU-seconds per second.

## Events, delivery and requests

| step | start (UTC) | sessions | events/s target / offered / committed | write errors non-2xx / transport | delivery ms from arrival p50 / p99 | delivery ms from commit p50 / p99 | issue-topic receipts got / expected | req/s target / sent | req 2xx | req ms p50 / p99 | requests dropped at the in-flight cap |
|---|---|---|---|---|---|---|---|---|---|---|---|
| step-10x | 2026-10-28 10:33:56 | 1,000 | 40.00 / 39.89 / 0.40 | 0 / 23,708 | 18,442 / 31,611 | 83 / 4,216 | 1,200 / 1,200 | 600.0 / 597.9 | 0.0% | 29,977 / 30,156 | 0 |

## Write latency by kind

Write ms p50 / p99 over 2xx writes, then committed / offered.

| step | comment ms p50 / p99 (committed / offered) | anchored ms p50 / p99 (committed / offered) | reply ms p50 / p99 (committed / offered) | edit ms p50 / p99 (committed / offered) | upload ms p50 / p99 (committed / offered) |
|---|---|---|---|---|---|
| step-10x | - (0 / 707) | - (0 / 7,846) | - (0 / 8,512) | 21,911 / 29,640 (171 / 5,239) | 14,121 / 23,399 (69 / 1,629) |

## Resources

| step | Dispatch cores mean / peak 5 s | Postgres cores | listener cores | NATS cores | loadgen cores | proxy cores | box busy | box busy before | Dispatch RSS max MiB | heap in use MiB p50 / max | goroutines p50 / max | goroutines waiting for a pool connection p50 / max | Postgres connections max | connections waiting on a lock p50 | Dispatch CPU-s per operation |
|---|---|---|---|---|---|---|---|---|---|---|---|---|---|---|---|
| step-10x | 0.29 / 0.48 | 0.27 | 0.062 | 0.026 | 0.52 | 0.06 | 62% | 0% | 1,771 | 960 / 1,272 | 38,844 / 39,718 | 19,283 / 19,753 | 79 | 18 | 0.0005 |

## Session delivery, listener and NATS

| step | session delivery asked | sessions on consumer / core | delivery ms from publish p50 / p99 | delivery ms from arrival p50 / p99 | issue-topic receipts lost | relay frames / duplicates / redelivered | listener cores first / last third | listener RSS MiB first / last third (max) | NATS cores first / last third | NATS RSS MiB first / last third (max) | JetStream consumers max | pull requests/s all / session | JetStream API requests/s |
|---|---|---|---|---|---|---|---|---|---|---|---|---|---|
| step-10x | consumer | 1000 / 0 | 6 / 56 | 18,442 / 31,611 | 0 | 1589 / 0 / 0 | 0.060 / 0.062 | 81 / 85 (96) | 0.027 / 0.025 | 143 / 155 (159) | 1,004 | 33.3 / 33.3 | 22.4 |

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
| step-10x | 18,442 / 31,629 | 328,978 / 608,337 | - | 367 | 0 |

## Ungranted locks

Every ungranted heavyweight lock in pg_locks, sampled once a second from 20 s into the window to its end. Up to four locks per step, most samples with a waiter first.

| step | samples with an ungranted lock | locks waited for: type target mode, samples with a waiter, waiters p50 / max | top waiter query → first blocker query (count), per lock |
|---|---|---|---|
| step-10x | 0% | - | - |

## Transaction ids

| step | transaction ids per s |
|---|---|
| step-10x | 0.2 |

## Requests by route

p50 / p99 ms (failures, non-2xx or transport, / count).

| route | step-10x |
|---|---|
| browser GET /api/v1/inbox | 29,977 / 30,156 (14,494/14,494) |
| browser GET /api/v1/issues/{key} | 29,977 / 30,157 (79,066/79,066) |
| browser GET /api/v1/issues/{key}/events?after=0&limit=200 | 29,977 / 30,162 (7,119/7,119) |
| browser GET /api/v1/issues?project={project} | 29,977 / 30,167 (21,385/21,385) |
| browser GET /api/v1/me/replies | 29,977 / 30,149 (10,755/10,755) |
| browser GET /api/v1/me/state | 29,977 / 30,133 (7,133/7,133) |
| cli GET /api/v1/agents | 29,978 / 30,144 (3,535/3,535) |
| cli GET /api/v1/issues/{key} | 29,977 / 30,148 (25,106/25,106) |
| cli GET /api/v1/issues/{key}/events?after=0&limit=200 | 29,977 / 30,145 (25,008/25,019) |
| cli GET /api/v1/issues/{key}/references | 29,977 / 30,164 (25,085/25,085) |
| cli GET /api/v1/references?from={key_ref} | 29,977 / 30,164 (21,461/21,461) |
| cli GET /api/v1/references?to={key_ref} | 29,977 / 30,167 (21,538/21,538) |
| other GET /api/v1/asks/open?project={project} | 29,977 / 30,170 (10,787/10,790) |
| other GET /api/v1/projects | 29,977 / 30,148 (10,829/10,833) |
| other GET /api/v1/search?q=outbox+latency | 29,978 / 30,141 (14,337/14,337) |
| pod GET /api/v1/artifacts/{artifact_id}/text | 29,977 / 30,150 (35,892/35,892) |
| pod GET /api/v1/issues/{key}/comments | 29,976 / 30,167 (10,705/10,705) |
| pod GET /api/v1/issues/{key}/comments?artifact=spec | 29,977 / 30,150 (14,459/14,461) |

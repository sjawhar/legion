# Load test tables

One row per step. Latencies are ms over the measured window; cores are CPU-seconds per second.

## Events, delivery and requests

| step | start (UTC) | sessions | events/s target / offered / committed | write errors non-2xx / transport | delivery ms from arrival p50 / p99 | delivery ms from commit p50 / p99 | issue-topic receipts got / expected | req/s target / sent | req 2xx | req ms p50 / p99 | requests dropped at the in-flight cap |
|---|---|---|---|---|---|---|---|---|---|---|---|
| step-10x | 2026-10-29 09:42:05 | 1,000 | 40.00 / 39.89 / 0.63 | 0 / 23,557 | 18,342 / 29,000 | 68 / 2,142 | 1,885 / 1,885 | 600.0 / 597.9 | 0.0% | 29,986 / 30,040 | 0 |

## Write latency by kind

Write ms p50 / p99 over 2xx writes, then committed / offered.

| step | comment ms p50 / p99 (committed / offered) | anchored ms p50 / p99 (committed / offered) | reply ms p50 / p99 (committed / offered) | edit ms p50 / p99 (committed / offered) | upload ms p50 / p99 (committed / offered) |
|---|---|---|---|---|---|
| step-10x | - (0 / 707) | - (0 / 7,846) | - (0 / 8,512) | 19,714 / 28,596 (282 / 5,239) | 10,591 / 18,410 (95 / 1,629) |

## Resources

| step | Dispatch cores mean / peak 5 s | Postgres cores | listener cores | NATS cores | loadgen cores | proxy cores | box busy | box busy before | Dispatch RSS max MiB | heap in use MiB p50 / max | goroutines p50 / max | goroutines waiting for a pool connection p50 / max | Postgres connections max | connections waiting on a lock p50 | Dispatch CPU-s per operation |
|---|---|---|---|---|---|---|---|---|---|---|---|---|---|---|---|
| step-10x | 0.50 / 0.93 | 0.52 | 0.054 | 0.033 | 0.80 | 0.14 | 68% | 1% | 2,562 | 1,540 / 1,998 | 39,136 / 39,870 | 19,353 / 19,718 | 80 | 58 | 0.0008 |

## Session delivery, listener and NATS

| step | session delivery asked | sessions on consumer / core | delivery ms from publish p50 / p99 | delivery ms from arrival p50 / p99 | issue-topic receipts lost | relay frames / duplicates / redelivered | listener cores first / last third | listener RSS MiB first / last third (max) | NATS cores first / last third | NATS RSS MiB first / last third (max) | JetStream consumers max | pull requests/s all / session | JetStream API requests/s |
|---|---|---|---|---|---|---|---|---|---|---|---|---|---|
| step-10x | consumer | 1000 / 0 | 5 / 56 | 18,342 / 29,000 | 0 | 2995 / 0 / 0 | 0.053 / 0.054 | 91 / 91 (91) | 0.031 / 0.035 | 143 / 159 (162) | 1,004 | 33.3 / 33.3 | 22.1 |

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
| step-10x | 18,362 / 29,060 | 320,109 / 625,476 | - | 380 | 0 |

## Ungranted locks

Every ungranted heavyweight lock in pg_locks, sampled once a second from 20 s into the window to its end. Up to four locks per step, most samples with a waiter first.

| step | samples with an ungranted lock | locks waited for: type target mode, samples with a waiter, waiters p50 / max | top waiter query → first blocker query (count), per lock |
|---|---|---|---|
| step-10x | 0% | - | - |

## Transaction ids

| step | transaction ids per s |
|---|---|
| step-10x | 0.8 |

## Requests by route

p50 / p99 ms (failures, non-2xx or transport, / count).

| route | step-10x |
|---|---|
| browser GET /api/v1/inbox | 29,986 / 30,041 (14,494/14,494) |
| browser GET /api/v1/issues/{key} | 29,986 / 30,039 (79,066/79,066) |
| browser GET /api/v1/issues/{key}/events?after=0&limit=200 | 29,986 / 30,045 (7,119/7,119) |
| browser GET /api/v1/issues?project={project} | 29,986 / 30,039 (21,385/21,385) |
| browser GET /api/v1/me/replies | 29,986 / 30,042 (10,755/10,755) |
| browser GET /api/v1/me/state | 29,986 / 30,041 (7,133/7,133) |
| cli GET /api/v1/agents | 29,986 / 30,035 (3,535/3,535) |
| cli GET /api/v1/issues/{key} | 29,986 / 30,042 (25,106/25,106) |
| cli GET /api/v1/issues/{key}/events?after=0&limit=200 | 29,986 / 30,040 (25,013/25,019) |
| cli GET /api/v1/issues/{key}/references | 29,986 / 30,039 (25,085/25,085) |
| cli GET /api/v1/references?from={key_ref} | 29,986 / 30,037 (21,461/21,461) |
| cli GET /api/v1/references?to={key_ref} | 29,986 / 30,039 (21,538/21,538) |
| other GET /api/v1/asks/open?project={project} | 29,986 / 30,039 (10,788/10,790) |
| other GET /api/v1/projects | 29,986 / 30,037 (10,833/10,833) |
| other GET /api/v1/search?q=outbox+latency | 29,986 / 30,041 (14,337/14,337) |
| pod GET /api/v1/artifacts/{artifact_id}/text | 29,986 / 30,039 (35,892/35,892) |
| pod GET /api/v1/issues/{key}/comments | 29,986 / 30,038 (10,705/10,705) |
| pod GET /api/v1/issues/{key}/comments?artifact=spec | 29,986 / 30,040 (14,454/14,461) |

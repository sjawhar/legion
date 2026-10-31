# Load test tables

One row per step. Latencies are ms over the measured window; cores are CPU-seconds per second.

## Events, delivery and requests

| step | start (UTC) | sessions | events/s target / offered / committed | write errors non-2xx / transport | delivery ms from arrival p50 / p99 | delivery ms from commit p50 / p99 | issue-topic receipts got / expected | req/s target / sent | req 2xx | req ms p50 / p99 | requests dropped at the in-flight cap |
|---|---|---|---|---|---|---|---|---|---|---|---|
| restart-resume-4 (listener exited) | 2026-10-29 16:28:47 | 1,000 | 40.00 / 39.69 / 0.94 | 0 / 11,631 | 16,656 / 30,166 | 70 / 2,226 | 1,405 / 1,405 | 600.0 / 596.1 | 0.2% | 29,983 / 30,052 | 0 |

(<process> exited): that stack process exited during the step (summary.json `stack_exits`), so the step failed and its row, in every table here, does not measure the load.

## Write latency by kind

Write ms p50 / p99 over 2xx writes, then committed / offered.

| step | comment ms p50 / p99 (committed / offered) | anchored ms p50 / p99 (committed / offered) | reply ms p50 / p99 (committed / offered) | edit ms p50 / p99 (committed / offered) | upload ms p50 / p99 (committed / offered) |
|---|---|---|---|---|---|
| restart-resume-4 (listener exited) | - (0 / 383) | - (0 / 3,848) | 24,857 / 24,857 (1 / 4,246) | 17,740 / 27,442 (212 / 2,636) | 11,202 / 14,771 (68 / 794) |

## Resources

| step | Dispatch cores mean / peak 5 s | Postgres cores | listener cores | NATS cores | loadgen cores | proxy cores | box busy | box busy before | Dispatch RSS max MiB | heap in use MiB p50 / max | goroutines p50 / max | goroutines waiting for a pool connection p50 / max | Postgres connections max | connections waiting on a lock p50 | Dispatch CPU-s per operation |
|---|---|---|---|---|---|---|---|---|---|---|---|---|---|---|---|
| restart-resume-4 (listener exited) | 0.48 / 0.89 | 0.52 | - | 0.045 | 0.80 | 0.13 | 68% | 3% | 2,491 | 1,479 / 1,892 | 38,880 / 39,572 | 19,227 / 19,550 | 81 | 5 | 0.0008 |

## Session delivery, listener and NATS

| step | session delivery asked | sessions on consumer / core | delivery ms from publish p50 / p99 | delivery ms from arrival p50 / p99 | issue-topic receipts lost | relay frames / duplicates / redelivered | listener cores first / last third | listener RSS MiB first / last third (max) | NATS cores first / last third | NATS RSS MiB first / last third (max) | JetStream consumers max | pull requests/s all / session | JetStream API requests/s |
|---|---|---|---|---|---|---|---|---|---|---|---|---|---|
| restart-resume-4 (listener exited) | consumer | 1000 / 0 | 5 / 75 | 16,656 / 30,166 | 0 | 6403 / 0 / 0 | - | - | 0.062 / 0.025 | 138 / 188 (188) | 1,004 | 33.1 / 33.1 | 19.6 |

Under consumer delivery a relay frame is a session's receipt, and the delivery latency from publish is its receipt time minus the stream's store time of the event (the frame's `Envoy-Stored-At`); on the core path it is `-`. Lost is the expected issue-topic receipts minus those got. A step whose sessions answered core measured the core path.

## Commit-order lock

Sampled every 20 ms from 20 s into the window to its end.

| step | time held | waiters p50 / max | hold ms p50 / p90 / p99 | holds seen | what the holder was doing (share of held samples) |
|---|---|---|---|---|---|
| restart-resume-4 (listener exited) | 1% | 0 / 4 | 719 / 1,600 / 1,600 | 7 | idle in transaction (Client:ClientRead) 95%; idle in transaction 3%; active:  update refs set source_seq = $3 where from_kind = $1 and from 1%; active:  with last_human as materialized ( select coalesce(max(id), 0) 1% |

## Delivery from sending, and live socket frames

NATS: every issue-topic receipt of a primary event. Live: the event frames of the dashboard live sockets that arrived in the window; from write response counts only the frames that carry the primary event of a committed in-window write with a 2xx status.

| step | NATS delivery ms from sent p50 / p99 | live frame ms from created_at p50 / p99 | live frame ms from write response p50 / p99 | live frames | live order violations |
|---|---|---|---|---|---|
| restart-resume-4 (listener exited) | 16,656 / 30,210 | 65,782 / 73,454 | 61,610 / 61,618 | 1,320 | 0 |

## Ungranted locks

Every ungranted heavyweight lock in pg_locks, sampled once a second from 20 s into the window to its end. Up to four locks per step, most samples with a waiter first.

| step | samples with an ungranted lock | locks waited for: type target mode, samples with a waiter, waiters p50 / max | top waiter query → first blocker query (count), per lock |
|---|---|---|---|
| restart-resume-4 (listener exited) | 3% | advisory commit_order_lock ExclusiveLock 2%, 0 / 3; transactionid row_wait ShareLock 2%, 0 / 1 | select pg_advisory_xact_lock($1) → select pg_advisory_xact_lock($1) (5); select status, closed_at is null from issues where key = $1 for no key update → select comment_id::text, target, delivery, resolved_session_id from comment_men (1) |

## Transaction ids

| step | transaction ids per s |
|---|---|
| restart-resume-4 (listener exited) | 1.5 |

## Requests by route

p50 / p99 ms (failures, non-2xx or transport, / count).

| route | restart-resume-4 (listener exited) |
|---|---|
| browser GET /api/v1/inbox | 29,983 / 30,053 (7,259/7,259) |
| browser GET /api/v1/issues/{key} | 29,983 / 30,052 (39,427/39,427) |
| browser GET /api/v1/issues/{key}/events?after=0&limit=200 | 29,983 / 30,058 (3,529/3,529) |
| browser GET /api/v1/issues?project={project} | 29,983 / 30,052 (10,577/10,577) |
| browser GET /api/v1/me/replies | 29,983 / 30,061 (5,309/5,309) |
| browser GET /api/v1/me/state | 29,982 / 30,055 (3,556/3,556) |
| cli GET /api/v1/agents | 29,976 / 30,013 (1,731/1,731) |
| cli GET /api/v1/issues/{key} | 29,983 / 30,047 (12,641/12,641) |
| cli GET /api/v1/issues/{key}/events?after=0&limit=200 | 29,983 / 30,052 (12,400/12,570) |
| cli GET /api/v1/issues/{key}/references | 29,983 / 30,049 (12,473/12,473) |
| cli GET /api/v1/references?from={key_ref} | 29,983 / 30,055 (10,653/10,653) |
| cli GET /api/v1/references?to={key_ref} | 29,983 / 30,057 (10,688/10,688) |
| other GET /api/v1/asks/open?project={project} | 29,983 / 30,047 (5,387/5,453) |
| other GET /api/v1/projects | 29,982 / 30,049 (5,296/5,352) |
| other GET /api/v1/search?q=outbox+latency | 29,983 / 30,050 (7,102/7,102) |
| pod GET /api/v1/artifacts/{artifact_id}/text | 29,983 / 30,055 (17,924/17,924) |
| pod GET /api/v1/issues/{key}/comments | 29,983 / 30,052 (5,373/5,373) |
| pod GET /api/v1/issues/{key}/comments?artifact=spec | 29,982 / 30,051 (7,144/7,224) |

# INFRA-594 load re-check of the pause backoff (2026-10-29)

The re-check of the earlier measurement, `.legion/INFRA-594/evidence/load2-results.md`, on the
branch head that added the paused relay's backoff.

- AFTER = legion/INFRA-594 `a93f11815d557c01e1e0b155a3130224c6182060` ("a paused session relay
  probes again after a backoff of 2 s doubling to 30 s, not only at the session's next
  registration"), `LOADTEST_SESSION_DELIVERY=consumer`. It is the earlier measurement's tree
  (`ae635cba`) plus that one fix and the evidence file.
- The tree is `git archive a93f1181 | tar -x` into `$TMPDIR/infra594/after`, its own
  `LOADTEST_ROOT` (`$TMPDIR/infra594/loadtest`), `stack.sh setup`, `stack.sh seed --force`:
  migration 136, 4 projects, 1,000 issues, 8,000 comments, 1,000 artifacts, 10,004 events, seeded
  in 626 s. The tree comes from `git archive`, so it carries no `.git` and `meta.json` records the
  build commit as `unknown`; the commit is the one named above.
- **The box is not the earlier measurement's.** This pod reports `nproc` 3 (`meta.json` `nproc`),
  where load2-results.md ran on 2 vCPUs. Every CPU share below is of three CPUs, and the
  cores, the box-busy shares and the latencies under saturation are therefore not strictly
  comparable with that report's columns. Postgres 16.2, kernel 4.19.0-gvisor, one run at a time.
- Two runs, each `step-10x` (40 events/s offered, 1,000 sessions, 600 requests/s, 20 SSE),
  `--duration 600s`, the default 30 s warm-up and 20 s grace. The second run carries the
  supplement beside it.

## 1. Steady state: one `step-10x` run, 600 s window

`keep/run-step-10x-steady/` (window 2026-10-29T09:42:05Z to 09:52:05Z, `box_busy_before` 1%).

| metric | this run | load2-results.md, after side (5 runs, median [min–max]) |
|---|---|---|
| box_busy_before % | 1.0 | 0.5 [0.3–1.1] |
| offered / committed per s | 39.9 / 0.628 | – / 0.608 [0.482–0.617] |
| from arrival p50 / p99, ms | 18,342 / 29,000 | 21,342 / 29,950 |
| from publish p50, ms | 5.29 | 6.45 [6.33–6.82] |
| from publish p99, ms | 56.3 | 33.3 [28.7–90.8] |
| from publish max, ms | 79.6 | 130 [39–198] |
| issue-topic receipts got / expected (lost) | 1,885 / 1,885 (0) | 0 lost in all runs |
| sessions on consumer delivery | 1,000 of 1,000 | 1,000 of 1,000 |
| relay frames / duplicates / redelivered | 2,995 / 0 / 0 | 2,307 [1,867–2,417] / 0 / 0 |
| relay gaps / undecodable / reply errors | 0 / 0 / 0 | – |
| `session relay paused` episodes in the listener log | 0 | – |
| listener cores (whole / 1st third / last third) | 0.054 / 0.053 / 0.054 | 0.040 / 0.041 / 0.040 |
| listener RSS max, MiB | 91.2 | 68.8 [66.4–74.1] |
| listener RSS slope, MiB/min | 0.00 | 0.15 [0.03–0.51] |
| nats cores (whole / 1st / last third) | 0.033 / 0.031 / 0.035 | 0.025 / 0.026 / 0.024 |
| nats RSS, MiB (1st third → last third, mean) | 143 → 159 | 140 → 151 |
| nats RSS max, MiB | 162 | 158 |
| nats RSS slope, whole window / last third, MiB/min | 2.45 / 1.50 | 1.97 [0.82–2.23] / 1.39 [0.83–1.83] |
| JetStream consumers max | 1,004 | 1,004 |
| session pull requests/s | 33.33 | 33.33 |
| JetStream API requests/s | 22.1 | 21.35 [20.81–21.44] |
| NATS slow consumers | 0 | – |

Nothing moved: every event reached every session that watches its issue, no frame was duplicated
or redelivered, every session stayed on consumer delivery, and no relay paused. The from-publish
p99 (56 ms against a median of 33 ms over the earlier five runs) is inside that spread's shape for
one run; the listener's cores and RSS are a little above the earlier side's, measured on another
box with a third CPU.

## 2. Supplement (labelled; not the spec's step): Dispatch-shaped envelopes published straight to NATS at 40/s, beside a second `step-10x` run

`keep/run-step-10x-supplement/` (window 09:56:32Z to 10:06:32Z, `box_busy_before` 3.5%),
`keep/supplement-publishes.jsonl.gz`, `keep/supplement-publisher.log`.

The publisher is `keep/tools/main.go` (`dispatchpub`), built under `$TMPDIR` against nats.go
1.50.0 only. It sends what Dispatch's outbox sends
(`internal/dispatch/outbox/publisher.go` `envelope()`): a `contracts.Envelope` with
`source: dispatch`, `event_id`/`dedupe_key`/`trace_id` `dispatch-<tag>-<id>`, `payload` the JSON of
a `model.Event` of type `comment.created` with a body of a few hundred bytes, published to
JetStream under `Nats-Msg-Id` `<dedupe key>:<topic>` as `bus.publishJetStream` does. Each envelope
goes on `notifications.dispatch.issue.<KEY>.comment.created` for one of the 1,000 seeded issues in
key order, round robin. Each of the step's 1,000 sessions watches 5 issues and each issue is
watched by exactly 5 sessions (`loadtest/run.go`, `sessions.go`), so every envelope is relayed to
5 session consumers: frames expected = publishes × 5. The event ids start at 9,000,000,000, far
above any id the step's own writes get, so the step's own numbers and the supplement's are told
apart in `relay-frames.csv` (`keep/tools/frames.py`). The from-publish time is the frame's
`recv_ns` minus its `Envoy-Stored-At` (`stored_at_ns`), the harness's own from-publish column.

The publisher starts when the step has registered its 1,000 sessions, that is in the warm-up, and
runs 600 s, so it stops about 30 s before the window ends and every frame it causes has the rest
of the window and the 20 s grace to arrive.

| metric | this run | before the backoff (load2-results.md, 1 run) |
|---|---|---|
| envelopes published / acknowledged | 22,817 / 22,817 | 24,056 |
| offered rate achieved | 38.0/s over 600.1 s | 40/s |
| frames expected / arrived | 114,085 / 114,085 | 120,280 / 120,280 |
| lost | 0 | 0 |
| duplicates | **0** | 82 |
| redelivered (`Envoy-Delivered` > 1) / gaps | 0 / 0 | – |
| from publish p50, ms | **42.2** | 129 |
| from publish p90 / p95, ms | 213 / 329 | – |
| from publish p99, ms | **668** | 2,146 |
| from publish max, ms | **1,570** | 84,823 |
| frames over 3 s (the relay's receipt wait) | 0 | – |
| `session relay paused` episodes in the listener log | **0** | 60 |
| the step's own events beside it: got / expected (lost) | 1,900 / 1,900 (0) | – |
| the step's own from publish p50 / p99 / max, ms | 5.09 / 38.9 / 57.6 | – |
| sessions on consumer delivery | 1,000 of 1,000 | – |
| listener cores / RSS max | 0.085 / 85.9 MiB | – |
| nats cores / RSS max | 0.203 / 218 MiB | – |
| NATS in / out msgs per s, slow consumers | 872 / 1,141, 0 | – |

Per minute of the window the supplement's from-publish p50 stays between 31 and 50 ms and its
worst frame in any minute between 608 and 1,570 ms: the tail does not grow over the ten minutes.

The earlier run's cause — "60 sessions on the saturated box answered `received` later than the
relay's 3 s wait; a paused relay resumed only at the session's next registration (up to 90 s) and
resent what was in flight" — did not arise here at all: no session took 3 s to answer a frame (the
worst frame of 114,085 was 1.57 s), so no relay paused, nothing was resent, and nothing was
duplicated. The backoff's own recovery path is therefore not exercised by this run; what this run
shows is that at the same offered rate the episodes that the backoff bounds no longer happen, and
that the worst delivery is 54 times faster than before (1.6 s against 84.8 s). The listener is
logging at INFO in this stack (9,331 INFO lines in `listener.log`), so a pause episode would have
been recorded.

The publisher reached 38.0/s rather than 40/s: its 25 ms ticker oversleeps on a saturated gVisor
box. The run therefore offered 5% fewer envelopes than the earlier supplement's 24,056.

## What is kept

| path | what it is |
|---|---|
| `keep/results.md` | this file |
| `keep/live-head.out`, `keep/live-head.table.txt` | the live session-delivery test at the head: stdout and its table |
| `keep/run-step-10x-steady/` | the steady run's step directory |
| `keep/run-step-10x-supplement/` | the supplement run's step directory |
| `keep/supplement-publishes.jsonl.gz` | one record per publish: event id, topic, send time, stream sequence |
| `keep/supplement-publisher.log` | the publisher's own result line |
| `keep/step-10x-supplement.run-step.out` | `run-step.sh`'s stdout for the supplement run |
| `keep/tools/` | `main.go`, `go.mod`, `go.sum` (the publisher), `frames.py`, `samples.py`, `run-supplement.sh` |

In both step directories `dispatch.log` and `goroutines.txt` (151 MB and 64 MB in the steady run)
were dropped, and `listener.log`, `run.log` and `heap.txt` gzipped, so the two directories hold
2 MB each instead of 233 MB. `summary.json`, `meta.json`, `TABLES.md`, `samples.json`,
`relay-frames.csv`, `nats.log`, the pprof profiles and their tops are all there.

Reproduce the two analyses with
`python3 keep/tools/frames.py <step dir> [--publishes <publishes.jsonl>]` and
`python3 keep/tools/samples.py <step dir>/samples.json` (gunzip the publishes file first).

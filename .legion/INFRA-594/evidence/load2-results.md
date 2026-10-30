# INFRA-594 load measurement on the merged harness (2026-10-28)

Copied from the measuring agent's report; the raw run directories were on the issue volume, which was recreated on 2026-10-29.

- BEFORE = main `c24ea5681ffb9924d7b18545f887dc9804b29dd4` (#2369 merged; core path).
- AFTER = legion/INFRA-594 `ae635cba200b2891eca9869e5192e74ec6aa1245`, `LOADTEST_SESSION_DELIVERY=consumer`.
- Each tree its own root, `stack.sh setup`, `seed --force` (migration 136, 1,000 issues, 8,000 comments). One 2-CPU gVisor pod.

## Steady state: `step-10x` (40 events/s offered, 1,000 sessions, 600 requests/s, 20 SSE), 600 s window, 5 runs per side, alternating

| metric | before | after |
|---|---|---|
| box_busy_before % | 0.8 [0.7–1.0] | 0.5 [0.3–1.1] |
| committed/s | 0.622 [0.600–0.692] | 0.608 [0.482–0.617] |
| from arrival p50 / p99, ms | 20,109 / 29,840 | 21,342 / 29,950 |
| from publish p50, ms | – | 6.45 [6.33–6.82] |
| from publish p99, ms | – | 33.3 [28.7–90.8] |
| from publish max, ms | – | 130 [39–198] |
| lost | 0 in all runs | 0 in all runs |
| listener cores (whole / 1st third / last third) | 0.019 / 0.020 / 0.019 | 0.040 / 0.041 / 0.040 |
| listener RSS max, MiB | 37.6 | 68.8 [66.4–74.1] |
| listener RSS slope, MiB/min | 0.04 [−0.12–0.10] | 0.15 [0.03–0.51] |
| nats cores (whole / 1st / last third) | 0.010 / 0.010 / 0.010 | 0.025 / 0.026 / 0.024 |
| nats RSS, MiB (1st third → last third) | 52.9 → 60.3 | 140 → 151 |
| nats RSS max, MiB | 66 | 158 |
| nats RSS slope, whole window, MiB/min | 1.16 [0.99–1.27] | 1.97 [0.82–2.23] |
| nats RSS slope, last third, MiB/min | 1.49 [0.68–2.18] | 1.39 [0.83–1.83] |
| JetStream consumers max | 4 | 1,004 |
| pull requests/s | 0 | 33.33 |
| JetStream API requests/s | 0.153 | 21.35 [20.81–21.44] |
| sessions on consumer delivery | – | 1,000 of 1,000 |
| relay frames / duplicates / redelivered | – | 2,307 [1,867–2,417] / 0 / 0 |

nats-server RSS grows over the window on both sides; the after side's whole-window slope is higher mostly in its first third (after 1,000 consumers register); in the last third both grow alike. A 10-minute window cannot show whether either levels off.

## Listener restart (after only): `step-10x`, 300 s window, listener SIGKILLed at +120 s, down 30 s, 3,000 envelopes published to the 1,000 sessions while down; times from the exec of the listener that stayed up

| metric | with resume points (n=5) | no resume points, `WARM=0 LOADTEST_NO_RESUME=1` (n=5) | no Dispatch requests, with resume (n=2) |
|---|---|---|---|
| every session has its backlog, ms: median [min–worst] | 3,900 [3,590–6,095] | 4,423 [3,922–8,399] | 4,860 [4,845–4,875] |
| per-session p99, ms | 3,899 [3,570–6,095] | 4,421 [3,918–8,399] | 4,854 |
| restart to first answer, ms | 1,360 [1,081–2,463] | 1,756 [1,155–2,038] | 1,882 |
| backlog lost / received twice | 0 / 0 | 0 / 0 | 0 / 0 |
| duplicates after the kill | 0 | 0 | 0 |
| JetStream API errors after the kill | 0 | 0 | 0 |

Before the restart fixes (ae635cba's parents before f523bba0 and 699f19d3): every session had its backlog 96 s after a restart, and sessions without a resume point lost 2,820 of 3,000 backlog messages.

## Supplement (labelled; not the spec's step): Dispatch-shaped events published straight to NATS at 40/s beside the step, after side, 1 run

24,056 events, 120,280 of 120,280 frames, 0 lost, 82 duplicates; from publish p50 129 ms, p99 2,146 ms, max 84,823 ms. Cause: 60 sessions on the saturated box answered `received` later than the relay's 3 s wait; a paused relay resumed only at the session's next registration (up to 90 s) and resent what was in flight.

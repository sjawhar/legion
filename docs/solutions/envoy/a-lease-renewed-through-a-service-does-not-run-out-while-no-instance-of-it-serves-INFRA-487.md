---
title: "A lease renewed through a service does not run out while no instance of it serves"
category: envoy
tags:
  - lease
  - heartbeat
  - outage
  - broker
  - sweeper
date: 2026-10-27
status: active
module: packages/envoy
related_issues:
  - "INFRA-487"
  - "sjawhar/legion#2269"
---

# A lease renewed through a service does not run out while no instance of it serves

- When a lease can be renewed only through service X, do not count time while no instance of X
  served against it. Otherwise an outage of X longer than the lease ends every client when X comes
  back, and the clients that end on a refusal (here `agent-secrets renew` exits 76) stay out until
  someone relaunches them.
- Put the liveness test in one SQL function, and call it in every statement that asks whether a
  lease is live: reads, renewals, and the sweep's selection. A lease that ran out after the last
  sign that X served, while no sign has come for longer than the threshold, is still live. When the
  signal comes back, give the gap back once, under a lock, and cap the result at one fresh lease
  past now. A revoked row, and a row that ran out while X *was* serving, stay ended.
- The "serving" signal must mean serving:
  - Start it beside the call that serves (`srv.Serve`), after every boot step that can fail. A
    fleet that crash-loops during boot must read as down, not as serving.
  - Keep one row per process, never one shared row. A writer stuck on its own held row then
    fails alone while the other instances keep the signal current. With one shared row, a held
    lock stops every instance and the guard turns on while X serves.
  - Bound every beat (a context deadline plus transaction-local `lock_timeout`,
    `statement_timeout` and `idle_in_transaction_session_timeout`), and log a failure with whether
    it timed out. An unbounded beat stalls silently.
  - Index the lookup of the latest row: the liveness function runs once per evaluated row
    (`docs/solutions/database-issues/a-sql-function-in-a-predicate-runs-once-per-row-so-index-every-table-it-reads-INFRA-487.md`).
- Refuse at boot a configuration in which a renewing client's lease could run out inside the gap
  the signal still counts as serving (here `2*BROKER_LEASE_SECONDS <= 9*BROKER_SWEEP_SECONDS`,
  since a client renews at a third of its lease and the threshold is three sweeps).
- State what the signal does not see: an instance that runs but that clients cannot reach (ingress,
  DNS, a NetworkPolicy) still counts as serving.

## Evidence

- The 2026-10-26 broker cutover: about 18 minutes with no broker, then 461 enrollments ended by the
  first sweep; every Legion role was relaunched by hand (dispatch INFRA-487).
- The fix: `enrollment_lease_live` in
  `packages/envoy/internal/broker/store/migrations/0019_broker_serving.up.sql`, used at nine sites in
  `internal/broker/enroll/enroll.go` and `internal/broker/requests/machine_{read,state}.go`;
  `enroll.ServingClock` in `internal/broker/enroll/serving.go`; the clock started beside
  `srv.Serve` in `cmd/broker/main.go`; the refusal in `internal/broker/config/config.go`.
- Review round 1 found the first version beat before `srv.Serve` and had one unbounded row
  (commit 301addfd fixed both). `TestABrokerThatDiesBeforeServingRecordsNoServingBeat` fails with
  the clock at its old place; `TestAHeldServingLockFailsTheBeatAndAnotherBrokerKeepsTheGuardOff`
  holds one process's row and shows the other keeps the guard off.
- `TestABrokerOutageLongerThanALease` (`cmd/broker/outage_test.go`), real broker and
  `agent-secrets` binaries, 58 s outage against a 45 s lease: the pod's fetch is served about
  100 ms after the restart, and it fails on the base commit with `proof invalid: not live`.

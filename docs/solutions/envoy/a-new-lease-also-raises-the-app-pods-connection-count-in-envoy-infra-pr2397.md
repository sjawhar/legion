---
title: "A new lease also raises the app pod's connection count in envoy-infra, and only envoy-infra-go checks it"
category: envoy
tags:
  - connection-pools
  - leases
  - envoy-infra
  - forward-merge
date: 2026-10-28
status: active
module: packages/envoy/infra/gcp
---

# A new lease also raises the app pod's connection count in envoy-infra, and only envoy-infra-go checks it

Extends docs/solutions/envoy/a-new-duty-exclusive-caller-raises-lease-pool-size-and-a-shortfall-is-silent-pr2233.md.

- Raising `leasePoolSize` raises `store.ConnectionsPerProcess` (`packages/envoy/internal/dispatch/store/pools.go`),
  and `dispatchConnectionsPerPod` in `packages/envoy/infra/gcp/dispatch.go` must equal it. Set both in
  the same commit.
- `TestDispatchConnectionsPerPodIsTheServers` is the only check. It lives in `packages/envoy/infra`,
  a separate Go module, so `go test ./...` from `packages/envoy` never runs it. Run
  `cd packages/envoy/infra && go test ./gcp/ -run TestDispatchConnectionsPerPodIsTheServers` yourself.
  In CI it is the `envoy-infra-go` job, which queues behind the rest and can stay unfinished for hours.
- After every forward merge, compute the number from the merged tree's pool constants, not from
  either side's literal. When main and your branch each added a lease, the right value is main's
  count plus yours, and it is on neither side.
- When the count rises, check that it still fits the database: every connection-holding app pod
  during a roll, times the count, must be under `max_connections` less 3
  (`packages/envoy/infra/README.md`, the `databaseTier` paragraph).

## Evidence

sjawhar/legion#2397 added the term-statistics refresh's lease. Review round 6 found
`dispatchConnectionsPerPod` still at 83 while `ConnectionsPerProcess` was 85; both review agents
reproduced the failure only in scratch copies of the envoy-infra module. While that fix was being
pushed, main's #2390 added the registry prune's lease and set the constant to 86 on its side; the
merged tree needed 87 (25c14fd6), and 497 / 87 still fits five pods on the default tier.

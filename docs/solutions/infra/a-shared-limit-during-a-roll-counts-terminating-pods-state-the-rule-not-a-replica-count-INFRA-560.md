---
title: "A shared limit during a roll counts the pods still terminating — state the rule, not a replica count, and read the per-pod figure from the module that owns it"
category: infra
tags:
  - kubernetes
  - rolling-update
  - max_connections
  - cloud-sql
  - pulumi
  - go-modules
date: 2026-10-27
status: active
module: packages/envoy/infra/gcp
related_issues:
  - "INFRA-560"
  - "sjawhar/legion#2368"
---

# A shared limit during a roll counts the pods still terminating — state the rule, not a replica count

Extends docs/solutions/infra/a-pre-stop-wait-is-paid-out-of-the-pods-grace-and-the-constant-that-sizes-it-lives-in-another-module-INFRA-62.md.

- **Count three kinds of pod against a shared limit while a Deployment rolls:** the replicas, the
  surge pods (`maxSurge`), and every old pod still inside its `terminationGracePeriodSeconds`. A
  Deployment does not count terminating pods against `replicas + maxSurge` (KEP-3973's
  `podReplacementPolicy`, which would, is alpha), and a terminating server keeps its connection
  pools until its shutdown closes them. So `(replicas + maxSurge) x per-pod` under-counts, and how
  many old pods overlap depends on how fast new pods turn ready. No finite bound follows from the
  Deployment alone.
- **Write the rule, not a computed replica count.** A doc or comment that says "this tier holds N
  replicas" is a derived number that goes stale when any input moves. Say which pods count, against
  what (for Postgres: `max_connections` less `superuser_reserved_connections`, 3 by default, which a
  non-superuser application user cannot use), and that the consumer sizes the limit for its replica
  count.
- **The per-pod figure is the owning module's exported sum.** Give the module that opens the
  connections one exported constant that sums every pool it opens (`store.ConnectionsPerProcess`),
  and say next to it that a new pool or a new long-lived connection goes into the sum. A dependent
  module reads that constant. If it keeps a copy, a test that compares the copy with the constant
  is the minimum, and it catches only a size change of a pool already counted.

## Evidence

- sjawhar/legion#2368 first stated "up to 14 replicas" for the default tier (32 connections per
  pod). After the forward merges brought in a shared pool of 64 and a lease pool of 9 it said 5
  (82 per pod). One more merge raised the lease pool to 10 (83 per pod), and the tester failed the
  round on the stale figure.
- A computed `dispatchDefaultTierRollingReplicas = (500 - 3) / 83 - 1` then failed review: at 4
  replicas a roll with one terminating pod already holds 6 x 83 = 498 > 497. The fix (`d31fcb18`)
  removed every stated bound and wrote the rule in `packages/envoy/infra/README.md`, the CHANGELOG,
  `gcp/dispatch.go` and `store/pool.go`. `store.ConnectionsPerProcess` (`pool.go`) and
  `TestDispatchConnectionsPerPodIsTheServers` stayed.

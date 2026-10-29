---
title: "The in-process NATS cluster restarts a mute member, and a single server is a false pass for a replica raise"
category: testing
tags:
  - nats
  - jetstream
  - cluster
  - testnats
  - flaky-tests
date: 2026-10-28
status: active
module: packages/envoy/internal/testnats
---

# The in-process NATS cluster restarts a mute member, and a single server is a false pass for a replica raise

Extends docs/solutions/testing/the-envoy-in-process-nats-rig-what-it-guarantees-and-the-three-places-it-does-not-INFRA-250.md.

- Prove clustered JetStream behaviour on `testnats.StartCluster(t, n)`, never on one server. A
  single nats-server v2.10.29 accepts an update raising an existing stream from 1 to 3 copies and
  records `Replicas: 3` while it holds one copy, so a raise "passes" there. Prove a refused raise
  on the cluster by asking for more copies than it has servers (5 on 3).
- Know which server version a fact holds on before a doc or test states it. nats-server v2.10.29,
  linked into the tests, refuses a new 3-copy stream with one of three servers stopped; v2.15.1,
  which the hub runs, creates it with one offline peer. The hub's version is checked with real
  binaries (`go run github.com/nats-io/nats-server/v2@v2.15.1`).
- A cluster member can start mute: `Cluster.AwaitReady` restarts a running member that no other
  member's meta group has heard from within 15 s, at most three times, and logs each restart. Do
  not raise the readiness timeout instead; a mute member never recovers.
- The helper's route relays hold every route port for the whole test, so a stopped member restarts
  on the same address and no port is bound and released for another process to take.

## Evidence

- nats-server v2.10.29 `setSystemAccount` starts the Raft send queue's goroutine
  (`server.go:1710`, `sendq.go:47-48`) before it stores the system account (`server.go:1717`).
  When the goroutine runs first, `registerWithAccount(nil)` fails silently and every Raft message
  the member sends is dropped (`client.go:3882`), so the leader never counts it. Under `-race`,
  `GOMAXPROCS=2` and four test processes at once, 29 of 1,920 cluster starts stuck this way and
  none recovered within 90 s; healthy starts were ready at p99 2.7 s, max 7.8 s. v2.15.2 passes
  the account in and fixes it.
- `packages/envoy/internal/testnats/cluster.go` (`AwaitReady`, `mute`, `muteAfter`); CI's
  `envoy-go-race` lane went from seven failing cluster tests to green
  (sjawhar/legion#2362, commit 7f536f31512a).

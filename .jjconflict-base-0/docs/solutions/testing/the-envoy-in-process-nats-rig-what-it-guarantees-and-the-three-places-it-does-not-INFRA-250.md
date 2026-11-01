---
title: "The Envoy in-process NATS rig: what it guarantees, and the three places it does not"
category: testing
tags:
  - nats
  - jetstream
  - test-isolation
  - testcontainers
  - envoy
date: 2026-10-25
status: active
module: packages/envoy/internal/testnats
related_issues:
  - "INFRA-250"
  - "sjawhar/legion#2189"
---

# The Envoy in-process NATS rig: what it guarantees, and the three places it does not

`packages/envoy/internal/testnats` runs nats-server **in the test process**. One server per test
binary, behind a TCP relay that holds the URL's port for the whole run, so `Stop`/`Start` keep the
URL and `Freeze` stops the bytes without a container. `testnats.URL(t)` hands each test — and each
subtest — a JetStream account no earlier test used.

Three things it does **not** give you. Each is silent.

## 1. The container guard is a door, not a wall

`TestNoPackageOutsideTheRigsImportsOrDrivesAContainerRuntime` reads `.Imports`, `.TestImports` and
`.XTestImports` — **direct** imports only — and allowlists the rigs `internal/testpg` and
`internal/tests3`. A package that imports an allowlisted rig therefore passes the guard and still
needs a container runtime: `cmd/listener/dispatch_resolution_test.go` imports `testpg`, and
`testpg.URL` starts a Postgres container whenever `DISPATCH_TEST_DATABASE_URL` is unset, as it is in
a Legion worker pod. The guard's own comment states the universal — *no package of this module
outside the rigs above may start a container* — which is more than it checks.

If you need the universal, walk `go list -deps`. If you do not, say "imports" in the name and the
comment. **Green here means no NATS container, not no container.**

## 2. `RecreateKeyValue` closes one window, and the package documents two

nats-server moves a deleted stream's directory aside and removes it from a goroutine.

- **Window B, guarded:** a create of the same name landing while that removal runs fails with
  `error creating store for stream` (`ErrCode 10049`). `testnats.RecreateKeyValue` retries exactly
  this.
- **Window A, unguarded:** a *second* delete of the same name before the goroutine has run leaves
  the files where they are **while answering success**, and the next create of that name recovers
  the deleted stream's messages. No error, wrong data.

`RecreateKeyValue` does not help with A, and nothing else does. Within one test, do not delete the
same name twice and then create it: use a second name, or take a fresh `testnats.URL` for the second
incarnation. Across tests the fresh-account-per-test contract already prevents it.

A retry is also the wrong instrument for a **restore**: a losing restore answers `ErrCode 10062`
(`restore failed: …`) on the end-of-data request, decoded as raw JSON rather than as a
`nats.APIError`, so a 10049 matcher never sees it. The general form of that trap is in
docs/solutions/architecture-patterns/derive-a-refusals-condition-from-what-the-caller-depends-on-not-from-the-mechanism-that-broke-it-INFRA-249.md:
a condition written around the mechanism that broke catches that mechanism and lets the next one
through. Here the caller's requirement is *the directory the create writes into is not being
removed* — which is why keeping another stream in the account closes the window, while matching an
error code only survives it.

## 3. There are two `testnats` packages, with opposite contracts

`packages/daemon/internal/testnats` has the same name, the same `URL`/`Main` call shape, and the
opposite isolation rule: it is still **container-backed**, and its `URL` holds a mutex for the test,
so a second take by that test **or by one of its subtests** fails rather than handing out a second
account. Envoy's `URL` returns the same URL to a repeat call by one test and a fresh account to each
subtest.

Check which one you import before relying on either rule — especially when moving a test between the
modules, or when porting this rig to the daemon.

## Two more facts worth knowing before you touch it

- **The NATS version the untagged suite tests is `packages/envoy/go.mod`'s**
  (`github.com/nats-io/nats-server/v2`), not `testnats.Image`. `Image` now moves only the
  `natslive` matrix lane. Since the modules share a `go.work`, another module's requirement can
  raise it; production code already depends on version-specific server behaviour, so read what moved
  before assuming a bump is inert.
- **`Gate`'s port is free while the gate is closed**, by design — that is what makes nats.go run
  `ReconnectErrCB`. `gatePorts` takes the 1000 ports below the machine's ephemeral low bound, read
  from `/proc/sys/net/ipv4/ip_local_port_range`, so an arbitrary `:0` bind will not land there. Two
  test binaries running concurrently share that band, so a long closed window in one can be taken by
  a gate in the other: keep closed windows short. The failure is loud (a `Fatal` from `Open`, or a
  reconnect-callback timeout), never a false pass.

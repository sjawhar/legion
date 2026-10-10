---
title: "A gVisor pod's reservation is the pod's sum, QoS reads cpu and memory alone, and ephemeral storage is an eviction fence over a small request"
category: legion
tags:
  - agent-sandbox
  - gvisor
  - kubernetes-resources
  - qos
  - ephemeral-storage
  - node-pool-sizing
date: 2026-10-09
status: active
module: packages/daemon/internal/daemon/kubernetes.go, packages/daemon/internal/config/kubernetes.go, packages/daemon/internal/runtime/sandbox
related_issues:
  - "LEGION-632"
  - "sjawhar/legion#1842"
---

# A gVisor pod's reservation is the pod's sum, QoS reads cpu and memory alone, and ephemeral storage is an eviction fence over a small request

- Under gVisor the per-container cgroup changes nothing inside the sandbox: `runsc` sizes the
  sandbox from the pod's cgroup, so inside any role container `nproc` is max(2, ⌈Σ cpu limits⌉) and
  `/proc/meminfo`'s `MemTotal` is about Σ memory of the pod's containers, and an active role may use
  what its idle siblings reserved. Assert a pod's reservation from inside against the pod's sum,
  never one container's (stage 4a `resources`: `nproc` and `MemTotal` within 3 % of the sums), and
  size a role's default for the pod it shares, not for the container the API shows. gVisor exposes
  no cgroup files, so there is nothing else to read in-pod.
- The kubelet's QoS class reads cpu and memory alone. `Guaranteed` needs each container's cpu and
  memory request equal to its limit; an ephemeral-storage request under its limit changes no pod's
  class. So the reservation is two shapes in one container: cpu and memory as one value each, and
  ephemeral storage as a limit over a smaller request (`checkReservation`,
  `internal/runtime/sandbox/sandbox.go`).
- Ephemeral storage is the node's disk a container's root filesystem writes to (`$HOME`, the state
  home with its browser profiles and logs, the Go and Bun caches a build fills); nothing else bounds
  it once pods of unrelated trees share a node. The limit is the eviction fence: a container past it
  has its own pod evicted, where node-wide `DiskPressure` evicts by the kubelet's ranking and can
  take another tree's pod. The request is what the scheduler counts against the node's allocatable
  ephemeral storage; a small default (1Gi) reserves almost nothing, so a node can be oversubscribed
  by the pods' limits by design, and `ephemeral_storage_request` is the operator's lever once the
  root volume is known. Do not "fix" the small request without that tradeoff
  (`defaultResources`, `internal/config/kubernetes.go`; docs/kubernetes.md "The node-disk bound").
- Every container of every Legion pod carries a reservation, the init containers and the image
  probe included, so the daemon's boot now depends on pool room for the probe pod (it carries the
  controller's reservation): count the probe and the controller's pod in any bound on concurrent
  pods, not issue pods alone (the reviewer's finding at #1842, left to a fast-follow).
- Size defaults from measurements in a real issue pod, under the `GOMAXPROCS` the pod will have,
  with a process-tree RSS sampler (`/usr/bin/time` is not in the worker image), and record the
  figures and the pin they came from in the handoff and the docs. Summed RSS double-counts shared
  pages (a headless Chromium's 4.2 GiB summed is 0.45 GiB in its largest process), so read the
  largest process beside the sum.
- A harness that asserts the daemon's defaults must read them from the daemon, not transcribe
  them: `scripts/e2e/lib/stage4b-reservations.ts` carries the table by hand and drifts the moment
  `defaultResources` moves — a golden the Go test writes and the e2e lib reads (as
  `packages/contracts/fixtures/daemon-api/version.json` is) is the fix, named in #1842's fast-follow.

## Evidence

LEGION-632 (#1842) gave every role container a reservation and measured the defaults in the issue's
own pod (2026-10-09, `GOMAXPROCS=3`): `go test ./...` of `packages/daemon` 1.45 GiB summed RSS,
cold `go build` 0.97 GiB, `bun test` of a plugin 1.2 GiB, the agent's own `omp` about 1 GiB after
half an hour, a headless Chromium's largest process 0.45 GiB — so a lane-running role's Go test
lane, agent and browser sum to about 3.9 GiB, which ran at the limit of the 4Gi first given the
implementer and tester; since a `Guaranteed` pod OOM-killed mid-turn stalls its tree, those two
defaults were raised to 6Gi (the reviewer keeps 4Gi), and a six-role pod sums to 3 CPU and 19 GiB,
still two per 8-vCPU floor node by cpu (7.9 / 3; 60.8 / 19 would place three). The reviewer's
node-disk finding (no bound on a role's root filesystem once trees share nodes) reinstated
ephemeral storage, which rounds 1–7 had refused as "the workspace lives on the volume": the
architect ruled it a per-role limit (20Gi implementer/tester, 10Gi others) over a 1Gi request, and
production (read 2026-10-09) showed one 700Gi gp3 root device per node with about 629 GiB
allocatable, two issue pods' limits summing to about 160Gi of it. The in-pod `nproc`/`MemTotal`
rule was confirmed live by stage 4a's `resources` checkpoint (PASS at 8129356d, 8cd761dc, 97c90a2f,
15dd07d9); the disk bound's eviction is exercised by no checkpoint (`review.json` `notProven`).

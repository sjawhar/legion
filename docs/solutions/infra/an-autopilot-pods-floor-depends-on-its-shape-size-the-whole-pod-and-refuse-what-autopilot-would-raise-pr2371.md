---
title: "An Autopilot pod's floor depends on its shape: size the whole pod and refuse what Autopilot would raise"
category: infra
tags:
  - gke-autopilot
  - resources
  - kubernetes
  - input-validation
date: 2026-10-29
status: active
module: packages/envoy/infra
related_issues:
  - "sjawhar/legion#2371"
---

# An Autopilot pod's floor depends on its shape: size the whole pod and refuse what Autopilot would raise

- **Read the floor for the pod's shape from Google's table.** Do not copy the floor of another
  component. On the general-purpose compute class without bursting, a pod with neither pod
  anti-affinity nor the `cluster-autoscaler.kubernetes.io/safe-to-evict` annotation has a minimum
  of 250m CPU. A pod with either one has a minimum of 500m. Memory is 512Mi for both
  (https://cloud.google.com/kubernetes-engine/docs/concepts/autopilot-resource-requests,
  "Minimums and maximums for compute classes"). A cluster with bursting goes lower, so these
  floors hold on either kind of cluster.
- **Compare the pod's total, sidecars included, with the floor and the memory-per-CPU ratio**
  (1 to 6.5 GiB per CPU). When the total is too low, Autopilot raises the request silently. When
  it is above the ceiling, Autopilot rejects the pod.
- **Refuse a size Autopilot would change, at resolve time, in a `core.InputError` that names the
  input and the rule** ("neither pod anti-affinity nor the safe-to-evict annotation"). Then the
  operator sees the real size before the preview, not a different size after the apply.
- **Write a test row at the boundary of each shape**: the smallest size accepted and the largest
  size refused. Use a row for a value that only the wrong floor refuses, for example `natsCpu:
  300m`. Such a row catches a floor copied from another component.
- **Keep the floor constants in one place.** `autopilotPodMinCPU` (500m), `autopilotPodMinMemory`
  and the ratio bounds are in `gcp/dispatch.go`. The hub adds only its own CPU floor beside them.

## Evidence

- `packages/envoy/infra/gcp/clusteredhub_autopilot.go`: `clusteredHubPodMinCPU` (250m) and
  `autopilotPodSize`, which adds the reloader's and the exporter's fixed sizes to the pod's total.
  gcp:Dispatch's pods carry pod anti-affinity, so its floor is `autopilotPodMinCPU`, 500m.
- The first version held the hub's pods to Dispatch's 500m. Because of that it refused
  `natsCpu: 300m` and a 150m listener with a false reason. Review thread `PRRT_kwDORFy7ds6xKafG`
  found this, and commit `16234e5e20b8` fixed it. The rows are in
  `gcp/clusteredhub_refusals_test.go`, and the acceptance is in `gcp/clusteredhub_test.go`.

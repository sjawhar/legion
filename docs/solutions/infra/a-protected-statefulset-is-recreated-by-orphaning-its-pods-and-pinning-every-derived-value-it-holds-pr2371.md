---
title: "A protected StatefulSet is recreated by orphaning its pods and pinning every derived value it holds"
category: infra
tags:
  - kubernetes
  - statefulset
  - pulumi
  - protect
  - disk-growth
date: 2026-10-29
status: active
module: packages/envoy/infra
related_issues:
  - "sjawhar/legion#2371"
---

# A protected StatefulSet is recreated by orphaning its pods and pinning every derived value it holds

- **Protect a stateful workload's StatefulSet.** Use Pulumi's `protect`, set through a chart
  transform when a chart renders it. A change to `volumeClaimTemplates`, `selector`, `serviceName`
  or `podManagementPolicy` replaces the StatefulSet and stops every replica at once. With
  `protect`, the preview fails.
- **To make such a change, write the manual path in the operator docs:** expand the claims; delete
  the StatefulSet object alone (`kubectl delete statefulset <n> --cascade=orphan`); remove it from
  the state (`pulumi state unprotect`, then `pulumi state delete`); then `pulumi up` creates it
  again, and its controller adopts the running pods.
- **Adoption holds only when the new pod template equals the running one.** If an input derives a
  template value from the field you changed (here the file-store limit, which by default derives
  from `diskSizeGb`), the new template differs and the controller replaces the pods you meant to
  adopt. In the same update, pin each derived value to what the pods run now. Read it from the
  pods (for example their annotation). Change it in a later update of its own, which rolls the
  pods.
- **State which steps ran where.** A procedure proven on kube-apiserver and
  kube-controller-manager with no kubelet has not proven claim expansion.

## Evidence

- `packages/envoy/infra/gcp/clusteredhub_kube.go`: `protectClusteredHubStatefulSet`, the chart
  transform.
- `packages/envoy/infra/README.md`, "The protected StatefulSet": the six steps. Step 5 pins
  `maxFileStoreGb` to the pods' `envoy-infra.sjawhar.dev/max-file-store` annotation, and step 6
  raises it alone.
- In the first version of the steps, every hub pod restarted, because the store limit followed
  `diskSizeGb`. Review thread `PRRT_kwDORFy7ds6wndgk` found this, and commit `f589463471e3` fixed
  it. The tester ran steps 3-6 against kube-apiserver and kube-controller-manager v1.33.0. Steps
  1-2 have not run on a cluster.

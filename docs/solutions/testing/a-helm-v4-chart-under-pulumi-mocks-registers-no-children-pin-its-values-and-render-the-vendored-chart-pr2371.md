---
title: "A helm v4 Chart under Pulumi mocks registers no children: pin its values and render the vendored chart"
category: testing
tags:
  - pulumi
  - helm
  - mocks
  - kubernetes
  - render-test
date: 2026-10-29
status: active
module: packages/envoy/infra
related_issues:
  - "sjawhar/legion#2371"
---

# A helm v4 Chart under Pulumi mocks registers no children: pin its values and render the vendored chart

- **Do not wait for chart objects in a mock test.** The Kubernetes provider renders a
  `kubernetes:helm.sh/v4:Chart`'s objects, and `pulumi.WithMocks` does not run that provider. A
  mock test therefore sees the Chart resource and none of its StatefulSet, ConfigMap or Service.
- **Under mocks, assert the Chart's inputs:** chart name, version, repository, namespace and the
  whole `values` map.
- **Export each resource transform through `export_test.go` and call it as a function.** Check
  the object it changes, and check that it leaves every other type unchanged. A transform never
  runs under mocks.
- **Prove what the values render with real `helm template`** on a vendored archive of the exact
  chart version the component names. Check the archive's SHA-256 so that the test fails when the
  copy drifts. Then assert the rendered objects, and run the rendered configuration where a real
  process can read it. Pin the helm binary's version too.
- **Cover a values change in both tests**: the mock test pins the input, and the render test pins
  its effect.

## Evidence

- `packages/envoy/infra/gcp/clusteredhub_kube_test.go` pins the chart's inputs, and calls
  `gcp.ProtectClusteredHubStatefulSet` directly (`gcp/export_test.go`).
- `gcp/clusteredhub_live_test.go` (`natslive` tag): `TestClusteredNatsHubChart` renders
  `gcp/testdata/nats-2.15.1.tgz` with helm v3.20.2 and the registration's values, with the
  SHA-256 checked. `TestClusteredNatsHubCluster` runs three nats-server processes on the rendered
  `nats.conf` and `auth.conf`.
- Only the render test sees the StatefulSet's `config.kubernetes.io/depends-on` annotation and
  the pod annotations as the chart merges them (`TestClusteredNatsHubChart` asserts both on the
  rendered StatefulSet).

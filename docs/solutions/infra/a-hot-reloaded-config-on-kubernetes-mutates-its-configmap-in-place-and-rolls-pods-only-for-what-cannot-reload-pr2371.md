---
title: "A hot-reloaded config on Kubernetes mutates its ConfigMap in place and rolls pods only for what cannot reload"
category: infra
tags:
  - kubernetes
  - pulumi-kubernetes
  - configmap
  - hot-reload
  - nats-server
  - helm
date: 2026-10-29
status: active
module: packages/envoy/infra
related_issues:
  - "sjawhar/legion#2371"
---

# A hot-reloaded config on Kubernetes mutates its ConfigMap in place and rolls pods only for what cannot reload

- **Build the component's own Kubernetes provider with `enableConfigMapMutable` on** when a pod
  mounts a ConfigMap whose data changes. By default pulumi-kubernetes replaces a ConfigMap whose
  data changes and deletes it first, so a pod scheduled in that window fails to mount its
  configuration. Only a provider the component builds can promise the setting; pin it in a unit
  test.
- **Turn off the chart's config-checksum annotation** (`configChecksumAnnotation: false` in the
  nats chart) so that a change the process reloads does not restart every pod.
- **List the values the process refuses to reload, and put each in a pod-template annotation**,
  so that a change of exactly those values rolls the pods one at a time. Read the reload code for
  the list: do not trust the docs. nats-server v2.15.1 refuses a new `no_auth_user` and a lower
  `max_file_store` (`server/reload.go`).
- **Assume a refused reload is sticky.** A nats-server reload is all or nothing against the
  options the server runs with. After one refused value is in the file, every later reload is
  refused too, including a change the server could reload alone. A pod left on such a file takes
  no further change until it restarts.
- **Apply the workload only after its ConfigMap**, every time and not only when a roll is
  expected. In a helm v4 Chart, put `config.kubernetes.io/depends-on:
  /namespaces/<ns>/ConfigMap/<name>` on the StatefulSet (core group empty; references within the
  chart only). Applied in parallel, a rolled pod can start on the old files, and a failed
  ConfigMap update can leave the pods rolled with nothing left to roll them again.
- **Prove the refusal on a real server.** Put a refused value next to a reloadable one in one
  change, and assert that both take effect only after a restart.

## Evidence

- `packages/envoy/infra/gcp/clusteredhub_kube.go`: `newClusteredHubKubernetes` builds
  `<name>-kubernetes` with `EnableConfigMapMutable` and `EnableServerSideApply`, pinned by
  `TestClusteredNatsHubBuildsItsOwnKubernetesProvider`. The `no-auth-user` and `max-file-store`
  annotations are `clusteredHubNoAuthUserAnnotation` and `clusteredHubMaxFileStoreAnnotation`. The
  depends-on value is set in `clusteredHubChartValues`.
- The first version rolled the pods only for a session-user change. Review found that a lower
  store limit blocked every later users reload. The fix is commit `90d28d94626a`, and the order
  rule without a session user is pinned in
  `TestClusteredNatsHubRollsThePodsOnlyForASessionUserOrStoreChange` (commit `f589463471e3`).
- The live test `gcp/clusteredhub_live_test.go`, phase (f), runs three nats-server 2.15.1
  processes. A lower `max_file_store` is refused at reload ("config reload not supported for
  decreasing jetstream max memory and store"). The users change made in the same update is
  refused with it, and both take effect after a restart.

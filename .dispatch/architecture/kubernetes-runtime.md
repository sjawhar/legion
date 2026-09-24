---
title: Kubernetes runtime and worker image
parent: legion
depends_on: [daemon, release-engineering]
paths: [deploy/kubernetes, scripts/kind-smoke]
---
How Legion runs under `runtime: kubernetes`: the Kustomize base and kind overlay that deploy the daemon, its network policies and operator controller config, and the throwaway-cluster proof in `scripts/kind-smoke` that walks eight checkpoints from admission to a resumed pod. Both pin the worker image `release-engineering` builds by digest, and the smoke reads `packages/daemon` sources directly to check the cluster it brought up matches the daemon it is testing. The image's own Dockerfile lives under `packages/daemon`. LEGION-206 plans replacing this hand-rolled pod layer with the kubernetes-sigs Agent Sandbox CRD, as Stage 4 of the Go coordinator — the same subsystem evolving, not a new component.

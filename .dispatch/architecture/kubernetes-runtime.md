---
title: Kubernetes runtime and worker image
parent: legion
depends_on: [daemon, release-engineering]
paths: [deploy/kubernetes]
---
How Legion runs under `runtime: kubernetes`: the pods of the worker image `release-engineering` builds, pinned by digest, and the operator's controller configuration (`deploy/kubernetes/daemon/controller.yaml.example`). The TypeScript daemon refuses `runtime: kubernetes` and names the Go daemon, which runs it. The image's own Dockerfile lives under `packages/daemon`. LEGION-206 plans replacing this hand-rolled pod layer with the kubernetes-sigs Agent Sandbox CRD, as Stage 4 of the Go coordinator — the same subsystem evolving, not a new component.

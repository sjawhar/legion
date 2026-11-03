---
title: Kubernetes runtime and worker image
parent: legion
depends_on: [daemon, release-engineering]
paths: [deploy/kubernetes]
---
How Legion runs under `runtime: kubernetes`: the pods of the worker image `release-engineering` builds, pinned by digest, and the operator's controller configuration (`deploy/kubernetes/daemon/controller.yaml.example`). The TypeScript daemon refuses `runtime: kubernetes` and names the Go daemon, which runs it. The image's own Dockerfile lives under `packages/daemon`. LEGION-206 plans replacing this hand-rolled pod layer with the kubernetes-sigs Agent Sandbox CRD, as Stage 4 of the Go coordinator — the same subsystem evolving, not a new component.

`deploy/kubernetes/daemon/legion.yaml.example` documents the daemon's top-level `repository_renames` key, the repository pairs the daemon treats as one repository wherever it compares a name; `cmd/legion/config_example_test.go` holds the example to the loader, so a key documented wrongly there fails that test rather than an operator's `--check-config`.

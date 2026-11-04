---
title: Kubernetes runtime and worker image
parent: legion
depends_on: [daemon, release-engineering]
paths: [deploy/kubernetes]
---
How Legion runs under `runtime: kubernetes`: the pods of the worker image `release-engineering` builds, pinned by digest, and the operator's controller configuration (`deploy/kubernetes/daemon/controller.yaml.example`). The TypeScript daemon refuses `runtime: kubernetes` and names the Go daemon, which runs it. The image's own Dockerfile lives under `packages/daemon`. LEGION-206 plans replacing this hand-rolled pod layer with the kubernetes-sigs Agent Sandbox CRD, as Stage 4 of the Go coordinator — the same subsystem evolving, not a new component.

`deploy/kubernetes/daemon/legion.yaml.example` documents the daemon's top-level `repository_renames` key, the repository pairs the daemon treats as one repository wherever it compares a name; `cmd/legion/config_example_test.go` holds the example to the loader, so a key documented wrongly there fails that test rather than an operator's `--check-config`.

A project's memory overlays (`projects.<KEY>.memory.config_files`) are files of the operator's mounts, as a role's own (`role_config_files`) are, and take part in everything those take part in: the loader's mount check, the image probe's per-role value (passed for every workflow role, never for the controller, which runs in no tree), the pod drift, reported `memory.config_files <file>`, which replaces every issue pod of the deployment and no controller pod, since any tree may be drawn into the memory group, and the filtering a launch that keeps a live pod does, so a memory-group role started into a pod built before the mount is told only the overlays that pod holds and starts rather than refusing. The project's `instructions` file is not a mount: the daemon reads it and the runtime inlines its text into the role's system prompt, since a pod reads no file of the daemon's. `deploy/kubernetes/daemon/legion.yaml.example` documents the `memory` block and the `operator-memory` volume and mount it points to, and `cmd/legion/config_example_test.go` uncomments all three and holds them to the loader, as it does `role_config_files`.

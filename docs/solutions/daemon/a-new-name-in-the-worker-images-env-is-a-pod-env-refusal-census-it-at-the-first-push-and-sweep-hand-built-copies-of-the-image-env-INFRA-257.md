---
title: "A new name in the worker image's ENV is a pod.env refusal: census it at the first push, and sweep hand-built copies of the image environment"
category: daemon
tags:
  - worker-image
  - imageEnv
  - operator-pod-env
  - contract-census
  - dockerfile
date: 2026-10-26
status: active
module: packages/daemon/docker, packages/daemon/internal/runtime/sandbox
related_issues:
  - "INFRA-257"
  - "sjawhar/legion#2222"
---

# A new name in the worker image's ENV is a pod.env refusal: census it at the first push, and sweep hand-built copies of the image environment

Extends docs/solutions/daemon/what-the-worker-image-owns-and-what-the-deployment-owns-an-image-env-the-operator-cannot-set-and-a-cold-pull-only-the-cluster-can-time-LEGION-578.md.

A name added to the final stage's `ENV` must also go into `imageEnv`
(`TestImageEnvIsWhatTheWorkerImageSets` enforces this). From that point,
`runtime.kubernetes.pod.env` setting that name is refused at `legion start`, at
`--check-config` and at daemon boot, whatever image digest the deployment pins. That makes the
change a contract change:

- **Census:** open the pull request with a `## Contract change census`, as
  docs/solutions/legion/a-contract-census-names-refusals-too-and-lists-every-hit-tests-and-the-prs-own-files-included-and-a-finite-field-is-an-enum-INFRA-229.md
  says. It needs three parts:
  - `rg -n '<NAME>'` across `packages scripts deploy docs/site skills AGENTS.md .github`, with
    every hit and its disposition;
  - the deployment repository's pod-env configuration and its test fixtures, named only as "the
    deployment repository";
  - a rollout line: the refusal is immediate, and it applies to every pinned digest.
- **Hand-built copies:** sweep every recipe that rebuilds the image environment by hand, such as an
  `env -i PATH=… HOME=… OMP_PROFILE=…` chroot line. Grep for a value that only the image's list
  carries (`OMP_PROFILE=legion`, `DO_NOT_TRACK=1`), and add the new name to each copy you find. No
  test holds those copies to the image. A stale copy gives a proof an environment no real pod has.

## Evidence

- **Finding 1:** review round 1 of PR #2222 blocked on the missing census. Adding `CGO_ENABLED` to
  `imageEnv` (`operatorpod.go:28`) turned an operator's `pod.env.CGO_ENABLED` from `Config OK` on
  main into a load-time refusal. The census written afterwards listed 61 hits in 14 files, all
  unaffected outside the change's own files. The deployment repository's pod-env blocks set no
  `CGO_ENABLED`. The reviewer re-ran the search before approving.
- **Finding 4:** the rootfs-and-chroot recipe in
  docs/solutions/testing/prove-a-published-worker-image-from-a-pod-by-unpacking-its-rootfs-and-chrooting-under-unshare-LEGION-764.md
  built its `env -i` list by hand. It lacked `CGO_ENABLED=0`, and also `COREPACK_DEFAULT_TO_LATEST=0`,
  which an earlier change had added to the image. A proof run with that recipe would have given a
  go/build reader cgo on, where the pod's own bash tool, which inherits the image ENV, gives it off.

---
title: "Prove a worker-image change with a Job on the published digest, the base's image, and controls inside the head image"
category: testing
tags:
  - worker-image
  - proof
  - kubernetes-job
  - negative-control
  - gvisor
  - image-size
date: 2026-10-26
status: active
module: packages/daemon/docker
related_issues:
  - "INFRA-257"
  - "sjawhar/legion#2222"
---

# Prove a worker-image change with a Job on the published digest, the base's image, and controls inside the head image

Extends docs/solutions/testing/prove-a-published-worker-image-from-a-pod-by-unpacking-its-rootfs-and-chrooting-under-unshare-LEGION-764.md.

When `kubectl auth can-i create jobs` answers `yes` in the role's projected namespace, run the
pull request's published image as a Job. Do not unpack the rootfs. A Job runs the image's own ENV
and PATH, and it can carry a sidecar, such as Postgres for a suite.

- **Admission:** create it with `kubectl create -f - -o name` and `generateName`; `apply` refuses
  `generateName`. Copy the worker container's security context: uid, gid and fsGroup 1000,
  `allowPrivilegeEscalation: false`, every capability dropped, RuntimeDefault seccomp, and
  `automountServiceAccountToken: false`. Read the namespace's build admission before the first
  run; `kubectl create --dry-run=server` reports a refusal without running anything. Put scratch
  on an `emptyDir` and point `TMPDIR`, `GOCACHE` and `GOMODCACHE` at it.
- **Pin by digest, then check it:** name the image as `ghcr.io/sjawhar/legion-worker@sha256:…`,
  taken from the `Worker Image` run's `Record digest` step. Require the pod's
  `status.containerStatuses[0].imageID` to name that digest.
- **One script for every image:** each check prints `CHECK <name>: got [x] want [y]`, and the
  script ends with `JOB-RESULT pass|fail` and exits non-zero on any mismatch. Read the result once
  the Job has a `Complete` or `Failed` condition: run `kubectl wait --for=condition=complete` and
  `--for=condition=failed` together and take the first that returns.
- **Negative control:** run the same Job and script on the base's own image.
  - Before a forward merge, take the newest image whose revision is an ancestor of the base, and
    name the revision and the input diff.
  - After a forward merge, take main's image of the merged base. The size baseline moves with
    the merge.
- **Controls inside the head image:** add one for every default the change claims to keep. A
  check that reads the same on the base image proves nothing about the change. Show that the
  default would flip without the change: the opt-in still takes effect, and a copy of the
  toolchain without your setting gives the other value.
- **Red control for a Dockerfile check:** copy the step's `RUN` from the head's Dockerfile and run
  it under `sh -eu` in a Job on the head image. Put the broken input at its real path with
  `unshare -r -m` and `mount --rbind <copy> <path>`. A copy at another path trips an earlier check
  in the step, and the red then proves the wrong check. With this you do not need the pushed red
  commit that docs/solutions/daemon/a-dockerfile-self-checks-red-control-is-a-pushed-commit-when-the-only-build-is-ci-LEGION-767.md
  describes.
- **Size:** read each image's linux/amd64 manifest from the registry and record the layer count,
  the sum of `.layers[].size`, and the layers whose digests differ. If you changed an apt line,
  the delta is the apt layer's. If other layers grew, look at them.

## Evidence

- **Head images:** INFRA-257 ran the race proof as Jobs on three head images, `infra-257-race-head-cjf6l`,
  `-merged-9vkl2` and `-r2-pz829`. Each was Complete with `JOB-RESULT pass`, and its imageID named
  the digest. Each ran `CGO_ENABLED=1 go test -race` on envoy's `./internal/bus/`, a race canary
  and the cgo-default checks, under gVisor as uid 1000.
- **Base images:** no image existed for the first base (`sha-81a3b5ef7b47` returned 404). The
  first negative control was therefore the release image of an ancestor, `0af6288a`, whose
  inputs differed from the base by two `package.json` version bumps. #2216 then merged, and the
  control became main's own image of the merged base. Main's image had shrunk from 1040.7 MiB to
  1007.7 MiB in between, so the final delta, +69.9 MiB, is measured against the merged base. At
  the merged head, the apt layer was +73,261,947 of the +73,278,694 bytes.
- **Controls inside the head image:** cgo read `0` on the base image too, but only because it had
  no C compiler. The head image's own controls showed the `0` comes from the change:
  `CGO_ENABLED=1 go build` records `CGO_ENABLED=1`, and a copy of `/opt/go` without the `go.env`
  line prints `1` under `env -i`.
- **Red control:** the tester first put a copy of `/opt/go` without the line beside the real one.
  The step's `test "$goroot" = /opt/go` failed first. With `mount --rbind` over `/opt/go`, the
  step stopped at the new check (`+ test 1 = 0`): Jobs `infra-257-tester-r2-bvknl` and
  `infra-257-tester-r2nc-2cbpl` on PR #2222.

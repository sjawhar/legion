---
title: "Prove a file the image build generates with a probe commit and its revert, read by crane and dpkg-query --admindir"
category: daemon
tags:
  - worker-image
  - proof
  - positive-control
  - crane
  - dpkg
  - github-actions
date: 2026-10-26
status: active
module: packages/daemon/docker
related_issues:
  - "INFRA-255"
  - "sjawhar/legion#2216"
---

# Prove a file the image build generates with a probe commit and its revert, read by crane and dpkg-query --admindir

Extends docs/solutions/daemon/a-dockerfile-self-checks-red-control-is-a-pushed-commit-when-the-only-build-is-ci-LEGION-767.md.

That note makes a red control a pushed commit. The delta is for a positive control on a generated
artifact ("adding X to an input puts X in the output") and for reading the result without docker:

- **Push the probe, then its `jj revert` once the probe's image run has completed, with the pull
  request open.** The probe adds one input (a package on the apt line) and nothing else; `jj
  revert -r <probe> --insert-after <probe>` undoes it in a new commit, never a rewrite of the
  pushed one. `jj diff --from <feature> --to <revert>` must print nothing, so the revert head is
  the content that merges. Each head gets its own `sha-<12 hex>` image.
- **Compare the two images with `LC_ALL=C comm -3`.** Two cold builds can pull different Debian
  archive states; `comm -3` shows drift as extra lines instead of hiding it in a pass. Expect only
  the probed name.
- **Read the image's packages without docker or a chroot.** `crane export --platform linux/amd64
  <ref> - | tar -x -C <dir> var/lib/dpkg/status usr/share/doc …` (crane through `go run
  github.com/google/go-containerregistry/cmd/crane@<version>`, caches under `$TMPDIR`), then the
  pod's own `dpkg-query --admindir=<dir>/var/lib/dpkg`. Write the proof's query differently from
  the build's filter (`${db:Status-Abbrev}` with awk against the build's `${db:Status-Status}` with
  sed), and add names that only one source would carry: a trixie-only package present
  (`libssl3t64`), a bookworm name and the build stages' packages absent (`libssl3`, `make`, `g++`,
  `unzip`, `xz-utils`).
- **Know which run a check settlement covers before you act on it.** Envoy's `checks settled`
  event keys `check_runs` by check name, and the `Docs` and `Worker Image` workflows both name their
  job `build`: a settlement's passed `build` can be the docs build, with the image's job not yet
  started. Read the image run itself (`gh run list --workflow worker-image.yaml --commit <sha>`).
- **A newer push cancels the pull request's image run.** Since 2026-10-27 `worker-image.yaml`'s
  per-PR group has `cancel-in-progress` on `pull_request`: a push while the probe's run is
  `pending`, `queued` or `in_progress` cancels it, so wait for it to complete before the next push.

## Evidence

sjawhar/legion#2216. Probe a924253140fb (`tree` added to the apt line) built `sha-a924253140fb`
(run 40248131216): its licence file listed 313 packages, equal to its installed set, `tree` among
them with its copyright file. The revert 8f7d8bd41fb0 built `sha-8f7d8bd41fb0` (run 40248164574),
and `comm -3` of it, of 4d6d8cb3's image and of 08418827's image against the probe each printed
only `tree`; all three named 312 = installed 312. The negative control, the released
`legion-worker:14.6.0`, named 0 against 312 installed. The settlement for 8f7d8bd at 00:25:19
reported `build` passed from `Docs` run 40248164555; the image's `build` job started at 00:25:24.
The simplify push (4d6d8cb3) went out while the revert's image job read `queued`; that run
completed and the new one waited behind it.

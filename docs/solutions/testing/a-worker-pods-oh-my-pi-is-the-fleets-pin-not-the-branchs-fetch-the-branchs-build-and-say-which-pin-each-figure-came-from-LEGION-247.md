---
title: "A worker pod's Oh My Pi is the fleet's pin, not the branch's: fetch the branch's pinned build, say which pin each figure came from, and expect the toolchain to vanish between phases"
category: testing
tags:
  - worker-pod
  - omp-pin
  - oh-my-pi
  - proof-environment
  - go-toolchain
  - jq
  - python3
  - real-binary-tests
date: 2026-10-07
status: active
module: packages/daemon
applies_when:
  - A real-binary suite or `legion probe-image` is run from a Legion worker pod
  - The pod's `$LEGION_OMP_PATH --version` differs from the checkout's `.omp-pin`
  - A phase worker is resumed for a later round and the tools it provisioned are gone
  - A repository check script needs `jq` or `python3` the worker image does not ship
related_issues:
  - "LEGION-247"
  - "sjawhar/legion#1831"
---

# A worker pod's Oh My Pi is the fleet's pin, not the branch's

Extends docs/solutions/testing/a-worker-pod-ships-no-go-or-tmux-provision-them-under-home-each-relaunch-and-record-every-figure-in-the-handoff.md.

- `$LEGION_OMP_PATH` in a pod is the build the worker image was made from, which is the fleet's
  current pin. The checkout's `.omp-pin` can name another build when the branch was cut before a
  pin bump. Compare `"$LEGION_OMP_PATH" --version` with `.omp-pin` before any real-binary run;
  when they differ, fetch the branch's build from the fork's public release (the asset
  `omp-v<pin>-linux-x64.tar.gz` on the tag `v<pin>`) under `$HOME` and run the suites on that,
  and record which pin each figure came from in the handoff and the PR body. A skill or settings
  API that moved between pins fails the probe on the wrong binary and proves nothing about the
  branch.
- After a forward merge that moves `.omp-pin` onto the pod's build, re-run the real-binary proofs
  on `$LEGION_OMP_PATH` and say so; the earlier figures were for another pin.
- The image ships no `go`, `jq`, `python3`, `mise`, `curl`, Docker or Postgres. Fetch with `bun`
  (`fetch` to a file), `uv` for a Python (`uv venv` plus `uv pip install pyyaml` for the
  `.github/scripts/check-*.sh` that read workflow YAML), and the Go tarball from go.dev; put a
  `python3` wrapper on `PATH` rather than a symlink, which a venv's launcher resolves wrongly.
  Name in the handoff which packages the pod cannot run at all (testcontainers, Postgres, a
  pod-safety test that reads the pod's own `/etc/legion-operator/overlay.yml`) and point at the
  CI run that covers them.
- `$HOME/tools` is gone when the role is resumed for a review round, not only on a relaunch: the
  recipe runs once per phase. Keep it to one shell line you can paste.

## Evidence

sjawhar/legion#1831: the pod's `omp` was 18.6.0 while the base commit's `.omp-pin` was
18.2.9-sami.20260924-172934; on 18.6.0 the gate's untouched skill resolution failed
(`settings.getGroup is not a function`), which a worker first read as a defect in the change. The
pinned 18.2.9 release was fetched from the fork's GitHub release and every real-binary Go case and
`legion probe-image` passed on it (`daemon-api-version=12`). The first forward merge of main
brought the pin bump and main's `probe.mjs` settings API, and the proofs were re-run on the pod's
18.6.0 (`daemon-api-version=12`, then `13` after the second merge). `go`, `jq`, a venv Python with
PyYAML and the pinned Oh My Pi were fetched into `$HOME/tools` in the first round and were gone at
the start of review round 1; the NATS-testcontainer, Postgres and one pod-safety test were named
in the handoff as CI-only.

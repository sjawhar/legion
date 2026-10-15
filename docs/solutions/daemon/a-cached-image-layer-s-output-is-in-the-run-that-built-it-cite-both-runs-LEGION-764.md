---
title: "A cached image layer's output is in the run that built it: cite both runs"
category: daemon
tags:
  - worker-image
  - github-actions
  - buildkit
  - cache
  - proof
date: 2026-10-15
status: active
module: packages/daemon/docker
related_issues:
  - "LEGION-764"
  - "sjawhar/legion#1967"
---

# A cached image layer's output is in the run that built it: cite both runs

Extends docs/solutions/daemon/omp-pin-bump-behavioral-proof.md (3c: a handoff-only commit re-runs the
`Worker Image` workflow fully `CACHED`, so the first run is the evidence). The cache is per layer, not per run:
a second code push that changes a later layer rebuilds from that layer down and prints `#<n> CACHED` for every
step above it, so the proof of one step and the proof of another can live in two runs at two heads.

- Before quoting a `RUN` step's output from a `Worker Image` log, find its step line (`#<n> [stage-5 2/22] RUN
  apt-get …`) and check whether the next line is `#<n> CACHED`. If it is, the output is in the newest run where
  that step was not cached; cite that run id and head beside the head's run, and say the step text is identical
  (the cache key is the instruction and its inputs).
- Count a package's `Setting up <pkg>` lines per stage: a build stage installing the same package (the
  `plugin` stage's `jq`, for its `omp.extensions` rewrite) prints one too, and it never reaches the runtime
  image. The runtime stage's lines are the ones that distinguish before from after; a `grep -c` over the whole
  log does not.

## Evidence

sjawhar/legion#1967, two pushes. Run 38765120996 at 60cf3e8a built the runtime apt layer (`#17 [stage-5 2/22]
RUN apt-get … jq openssh-client …`) and its log holds `Setting up jq (1.7.1-6+deb13u4)` and `Setting up
openssh-client (1:10.0p1-7+deb13u4)` there, plus the `plugin` stage's own `Setting up jq` at `#21`. The simplify
head 40b71edb reordered one command in the toolchain step, a later layer: run 38766268187's log shows `#77 CACHED`
for the apt step and no `Setting up` line at all, while its `#93` (`jq-1.7`, the `(ED25519)` fingerprint) and `#97`
(`probe-image: capability toolchain: present (… /usr/bin/jq, /usr/bin/ssh-keygen; …)`, `probe-image: OK`) are the
head's. The pre-change control, run 38759109678, has one `Setting up jq` (the plugin stage) and no
`openssh-client`; its toolchain row ends at `/usr/local/bin/kubectl;`. The PR body's `E2E (implementer)` line and
the handoff's proof entry cite both runs by step number, and the tester and reviewer confirmed each from GitHub.

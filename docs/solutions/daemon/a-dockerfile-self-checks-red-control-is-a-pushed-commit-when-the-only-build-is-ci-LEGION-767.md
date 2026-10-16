---
title: "A Dockerfile self-check's red control is a pushed commit when the only build is CI"
category: daemon
tags:
  - worker-image
  - github-actions
  - negative-control
  - red-run
  - dockerfile
  - debian
date: 2026-10-15
status: active
module: packages/daemon/docker
related_issues:
  - "LEGION-767"
  - "sjawhar/legion#1973"
---

# A Dockerfile self-check's red control is a pushed commit when the only build is CI

The worker image is built only by the `Worker Image` run on a pull request head (`worker.Dockerfile`'s
header; a pod has no docker). A spec that asks for "the self-check fails when the fix is removed, run
once before the fix lands" is therefore two pushes, not a local run.

- Push the guard without the fix first. Its run must fail in the guard's step; record the run id and
  the lines the step printed. That run is the negative control the PR body's `E2E` line and the
  handoff's `proof` cite. Then push the fix; its run is the positive proof. "One commit on top of
  `main`" in a spec is the squash result, not the branch history: say so in the plan so the reviewer
  and tester expect one red run.
- Before the red push, read what the red can move. Check the workflow is not in the base's required
  set (`gh api repos/<owner>/<repo>/rules/branches/main`; `Worker Image` is not, so its red moves no
  issue), and that the red lands during implementing, which the daemon's CI-red move never touches.
  Read the job's `concurrency`: `worker-image.yaml` serializes runs per pull request with
  `cancel-in-progress: false`, so the fix push queues behind the red run instead of cancelling it (a
  third push while one is still pending would cancel the pending one).
- Make the guard print what it saw, not just fail. Anchor it with a `stat -c '%a %n'` on each path
  (a moved or missing path fails the build instead of passing an empty `find`, and the modes land in
  the log), then capture `find <paths> -perm /6000` into a variable, `echo` it with a `none` default,
  and `test -z` it. The red run's log then shows `4755 …`, `2755 …`, `set-id bits: <paths>` above
  `exit code: 1`, and the green run's shows `755 …`, `set-id bits: none`.
- Enumerate the guard's targets from the package, not from the spec. A Debian package's data tar
  carries the modes it ships, and its maintainer scripts can add more after unpacking: trixie's
  `openssh-client` ships `/usr/lib/openssh/ssh-keysign` 4755 and its postinst runs `chgrp _ssh; chmod
  2755 /usr/bin/ssh-agent`. `apt-get download <pkg>` (a user-writable `Dir::State`/`Dir::Cache` in a
  pod), then `dpkg-deb -c` for the modes and `dpkg-deb -e` for `DEBIAN/postinst`, finds both; a guard
  scoped to the spec's one file would have failed the fixed image on the other.
- A fix inside the apt `RUN` rebuilds every layer below it, so the fix push's run is cold and is the
  one whose later steps print; the next head's run reuses them and prints `CACHED`
  (docs/solutions/daemon/a-cached-image-layer-s-output-is-in-the-run-that-built-it-cite-both-runs-LEGION-764.md).
  Cite both runs.

## Evidence

LEGION-767 (sjawhar/legion#1973). The spec named `chmod u-s /usr/lib/openssh/ssh-keysign` and a
self-check refusing any set-id bit under `/usr/lib/openssh` or on `/usr/bin/ssh-agent`; the planner's
`dpkg-deb -e` read of `openssh-client_10.0p1-7+deb13u4` found the postinst's `chmod 2755
/usr/bin/ssh-agent`, so the plan added `chmod g-s` as a recorded departure, which the architect
confirmed. Push one (26fae221, the guard alone): `Worker Image` run 38780999901 failed in the final
step printing `4755 /usr/lib/openssh/ssh-keysign`, `2755 /usr/bin/ssh-agent`, `set-id bits:
/usr/lib/openssh/ssh-keysign` `/usr/bin/ssh-agent`, `exit code: 1`; the apt layer was cached, so no
`Setting up` line. Push two (e624d9fd, the two `chmod`s in the apt `RUN`): run 38781816169, a cold
build, printed the toolchain step's `jq-1.7` and `(ED25519)` fingerprint and the final step's `755`,
`755`, `set-id bits: none`, then `probe-image: OK`. The simplify head 11e8dd57 (a Go test edit) ran
38782814273 with the toolchain layer `CACHED` and the same final-step lines; the PR body cites both runs
and the red one. Required checks on `main` were `lint`, `typecheck`, `test`, `pr-title`; the red run
moved nothing.

---
title: "A shell script that is a container's PID 1 traps TERM, sleeps under wait, and exits when the background helper it runs dies"
category: infra
tags:
  - kubernetes
  - pid-1
  - sigterm
  - shell
  - busybox
date: 2026-10-27
status: active
module: packages/envoy/infra/gcp
related_issues:
  - "INFRA-560"
  - "sjawhar/legion#2368"
  - "INFRA-549"
---

# A shell script that is a container's PID 1 traps TERM, sleeps under wait, and exits when its helper dies

Extends docs/solutions/testing/crash-a-pod-from-the-node-kubectl-exec-kill-9-1-is-dropped-and-judge-the-kill-from-the-pod.md.

- A container whose command is `/bin/sh -c '<script>'` runs the shell as PID 1, and Linux delivers
  a signal to a PID namespace's init only when init handles it (pid_namespaces(7)). Without a
  handler the kubelet's SIGTERM is dropped and the pod waits out its whole grace period for SIGKILL
  on every roll and drain. Put `trap 'exit 0' TERM` at the top.
- Write every sleep as `sleep N & wait "$!"`. A shell runs a trap only after a foreground command
  returns, so `sleep 60` delays the exit by up to 60 s; `wait` returns as soon as a trapped signal
  arrives.
- When the script runs a background loop it depends on (`( while :; do gate; sleep 30 & wait "$!"; done ) &`),
  keep its pid and check it with `kill -0` on each pass of the foreground loop; when it is gone,
  log one line naming it and `exit 1`, so the kubelet restarts the container and the restart count
  shows it. Nothing else restarts a dead background loop.
- Test it with the production script: send SIGTERM while the loop sleeps a real sleep and require
  exit 0 within a few seconds. The test's shell is not PID 1, so without the trap it dies of the
  signal; keep both negative controls (no trap, foreground sleep).

## Evidence

- In sjawhar/legion#2368 the front door's `advertiser` and `cert-publish` containers ran shell
  scripts as PID 1 with no trap. INFRA-549's review found it in the shared
  `packages/envoy/infra/gcp/tailnet_service.go`; the fix is INFRA-549's `8bf4bfe7`, copied byte for
  byte in `09b9025b`, and `cab89af3` for `cert-publish` (`dispatch_frontdoor.go`,
  `dispatchCertPublishLoop`).
- `TestDispatchCertPublishExitsOnSIGTERM` failed with "killed by SIGTERM rather than exiting on it"
  without the trap and with "still runs 5s after SIGTERM" with a foreground `sleep 60`.
- The `kill -0` check came from review round 1 (`5d50ff69`): the claim gate ran as a background
  subshell, and its death left the pod unready with nothing deciding.

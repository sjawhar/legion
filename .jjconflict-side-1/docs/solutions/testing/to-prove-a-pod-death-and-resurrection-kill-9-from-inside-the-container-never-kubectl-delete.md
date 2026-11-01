---
title: "To prove a pod death and its resurrection on kind, kill -9 the agent then the shim from inside the container; kubectl delete is a graceful exit the root self-reports and the daemon never resurrects"
category: testing
tags:
  - kubernetes-runtime
  - kind
  - resurrection
  - hard-kill
  - worker-shim
  - process-exit
  - production-like-proof
date: 2026-09-15
status: active
module: packages/daemon/src/cli/worker-shim.ts, packages/pi-envoy/extensions/legion.ts, packages/daemon/src/daemon/processes.ts
related_issues:
  - "LEGION-178"
  - "LEGION-81"
  - "sjawhar/legion#1121"
  - "sjawhar/legion#1108"
symptoms:
  - "after `kubectl delete pod <root>` (with or without `--grace-period=0 --force`) the daemon's state shows the tree `dead` with `locator: null` and no `resurrecting <KEY>` log line ever appears"
  - "a kind proof meant to exercise the daemon's death-recovery path instead exercises its clean-exit path"
---

# To prove a pod death, `kill -9` from inside the container — `kubectl delete` is a clean exit

## Why `kubectl delete` proves the wrong thing

Every form of `kubectl delete pod`, including `--grace-period=0 --force`, ends with the kubelet
sending **SIGTERM** to the pod's PID 1. In a Legion worker pod PID 1 is `legion worker-shim`,
whose SIGTERM handler (`worker-shim.ts`, `terminate()`) does exactly what the daemon's own
`shutdown` frame does: it closes OMP's stdin so OMP finishes its turn and exits cleanly. On the
way out the root architect's extension runs its `session_shutdown` hook
(`packages/pi-envoy/extensions/legion.ts`) and posts **`/process/exit`** to the daemon. For an open
issue that route is `ProcessManager.markProcessDead` → `recordRootExit(…, "dead")`: the tree's
status becomes `dead`, its locator is cleared (the session file kept in `resumeSessionFile`), its
admission slot is released, and nothing is relaunched. Resurrection belongs to the resync probe,
which probes only trees that still hold a locator — so a self-reported exit is never resurrected.
LEGION-81's run saw exactly this ("still self-reported", PR #1108) and had to re-admit the issue
by hand.

That is the intended behaviour for a process that exits on purpose. It is the wrong path for a
proof whose acceptance is "the daemon detects a dead pod and resurrects the same session", which
is LEGION-178's acceptance 1 (a resurrected root's second-generation pod must provision on the
existing clone).

## The kill that produces a real death

```sh
kubectl --context kind-<cluster> -n legion exec <root pod> -c worker -- sh -c 'kill -9 -1; kill -9 1'
```

Order matters. `kill -9 -1` signals every process the caller may signal **except PID 1** — the
kernel excludes the namespace's init from `-1` (kill(2)) — so it takes OMP down first, before any
hook can run; `kill -9 1` then kills the shim. Nothing self-reports. Observed in the LEGION-178 run
(digest `sha256:ba1b0242…6060d`): the worker container terminated `exitCode 137`, the pod went
`Failed`, and about 40 s later (`resync_interval_seconds: 60`) the daemon logged

```
[legion] pod legion-<key>-architect-g1 Failed: last lines of worker: …
[legion] resurrecting <KEY> by resuming OMP session /home/legion/.omp/profiles/legion/agent/sessions/…/<S1>.jsonl
```

and generation 2 came up with `--resume=<the same S1 file>`, `trees[KEY] = {status: active,
generation: 2, launchFailures: 0, readyConfirmedAt: <set>}` and the architect's `sessionId`
unchanged. The tester's independent run reproduced every line.

## If it still self-reports

`kill -9 1` alone leaves a small race for the same self-report (OMP may run its hook before the
shim's death is noticed). If three minutes after the kill the state shows `status: dead`,
`locator: null` and no `resurrecting` line, the kill was graceful: re-admit the issue (Dispatch
`backlog` → `todo`, relay the events) and record that generation 2 came through re-admission —
its `--resume` and init-container provisioning are the same code path, so a provisioning proof
still stands; a *resurrection* proof does not.

## Reading the evidence

- `kubectl get pod <g1> -o jsonpath='{.status.containerStatuses[0].state}'` → `terminated`,
  `exitCode 137`, `reason Error`: the SIGKILL landed.
- The daemon log's `Failed: last lines of worker:` line is the probe's verdict; `resurrecting`
  is the recovery. `failed to connect architect shim socket on ready … negotiate_protocol timed
  out` lines around it are documented noise on roots, not a failure.
- Any debug pod you leave running on the tree PVC must **not** carry the `legion.dev/project`
  label — the daemon's orphan sweep deletes every unknown pod that does (LEGION-177 lost a
  generation to this). The one-shot `kubectl run … --rm --restart=Never` state-curl pod needs the
  label for the NetworkPolicy and is fine because it ends at once.

## Related

- `../daemon/a-repository-git-config-serves-one-caller-another-overrides-it-through-git-config-env-pairs.md`
  — the bug this kill was used to reproduce and prove fixed.
- `packages/daemon/src/daemon/AGENTS.md`, `/process/exit` and the process-failure-recovery
  bullet — the two paths in full.

---
title: "Crash a pod from the node: `kubectl exec … kill -9 1` is dropped inside the PID namespace; judge that the kill landed from the killed pod, and let only the resurrection budget decide the rest"
category: testing
tags:
  - kubernetes
  - kind
  - pid-namespace
  - sigkill
  - crictl
  - resume
  - smoke-rig
  - kind-smoke
date: 2026-09-15
status: active
module: scripts/kind-smoke/checkpoints.sh (crash_root_pod, try_kill_landed, try_kill_resumed)
applies_when:
  - A smoke or e2e check must crash a container process for real, not delete its pod gracefully
  - A "did it die / is it gone" predicate reads the Kubernetes API in a poll
  - A resume checkpoint has to tell a crash the daemon did not recover from other work continuing
related_issues:
  - "LEGION-26"
  - "sjawhar/legion#1114"
  - "LEGION-182"
---

# Crash a pod from the node, and judge the kill from the pod

The kind smoke's `kill-pod-resume` checkpoint crashes the root architect's pod and waits for the
daemon to resurrect the same agent (`--resume`, generation +1). Three things about that kill cost a
round each.

## 1. `kubectl exec <pod> -- kill -9 1` does nothing

The kernel delivers to a PID namespace's init (the container's PID 1 — here the `legion
worker-shim` that fronts OMP) only the signals it has installed handlers for, when they come from
inside its own namespace; SIGKILL and SIGSTOP from inside are dropped. Only an ancestor namespace
can SIGKILL it. On kind the ancestor is the node container, so the rig resolves the container's
host pid there and kills from there:

```bash
container="$(pod_json "$pod0" | jq -r '.status.containerStatuses[]? | select(.name == "worker") | .containerID // empty' | sed 's|^containerd://||')"
node="$(kind get nodes --name "$cluster" | head -n1)"       # single-node kind; a multi-node cluster reads .spec.nodeName
hostpid="$(docker exec "$node" crictl inspect -o go-template --template '{{.info.pid}}' "$container")"
docker exec "$node" kill -9 "$hostpid"
```

The fallback is `kubectl delete pod --grace-period=0 --force`. Whichever fired is recorded in the
verdict (`kill_method`), because the two prove different things: the SIGKILL is a crash the process
never saw; a delete — even a forced one — is the control plane removing the pod. A *graceful* delete
is a third thing and not a crash at all: the shim forwards the shutdown, OMP exits cleanly and the
root reports its own exit, which is a different daemon path from the resync probe finding a dead
locator.

## 2. "The kill landed" is read from the killed pod, never from the tree moving

The first version decided the kill had worked when the tree's generation advanced or a claim
changed. A tree moves for many reasons — a phase worker finishing, a planner spawning — and none of
them says the architect died. `try_kill_landed` reads `pod0` itself: `Failed`, or the `worker`
container terminated with exit 137, or `NotFound` (the forced-delete fallback). While `pod0` is still
`Running` and the tree moves on, the verdict is immediate and names the method:
`the kill did not land: pod <pod> is still Running after <kill method> …`.

The read keeps kubectl's stderr. Only a `NotFound` is "gone"; a connection refused, a timeout, or
a stale kubeconfig is `could not read pod <pod>: <stderr>`, retried within the budget and named at
expiry. The earlier `pod_json … || echo '{}'` helper turned every API blip into an empty phase,
which read as "gone" — a landed verdict on a live pod. A `|| echo '{}'` reader is for reads where
absence and error are the same answer; it is never for a landed/gone judgement.

## 3. Once landed, tree movement is not a verdict; only the budget is

After the kill has landed, the tree moving at the recorded generation is other work continuing — a
phase worker's completion arriving while the root is dead — and proves nothing about resurrection.
The poll keeps going and only `SMOKE_WAIT_KILL_RESUME` (one resync interval plus a pod start)
decides: `no replacement within <budget>s: the daemon did not resurrect the root (recorded
generation N, current N; pod0 <landed>, locator <pod>; its resync probe should have — see the
daemon log)`. "Keeps working afterwards" is then judged against a snapshot taken once the
replacement has registered, not against the kill-time snapshot, so a change the dying architect
set in motion is not credited to the resurrected one.

## When to run it

Right after `tree-moved`, while the tree is mid-phase. A kill on an idle tree at `retro` resumed
correctly on the tester's run and then waited 1800 s for a move that never came — a true
observation of LEGION-182 (a root crashed while a phase completion is in flight never learns of
it), not a resume failure. A checkpoint that asserts a phase is scheduled relative to the lifecycle,
and its doc says so.

## Related

- `docs/solutions/daemon/a-crashed-phase-worker-is-retired-until-something-addresses-it-a-crashed-root-is-resurrected-by-resync.md`:
  why the target is the root architect and not a phase worker.
- `docs/kubernetes.md`, "Runbook: the kind smoke": the three `kill-pod-resume` failure rows.

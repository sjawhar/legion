---
title: "worker_cap is not a pod count: design outside checks against what the redacted state page and the pods actually expose"
category: daemon
tags:
  - worker-cap
  - worker-admission
  - redacted-state
  - kubernetes
  - pod-labels
  - smoke-rig
  - kind-smoke
date: 2026-09-15
status: active
module: packages/daemon/src/daemon/api/state.ts, worker-admission.ts, k8s-manifests.ts; scripts/kind-smoke/checkpoints.sh
applies_when:
  - Writing a check that reads `GET /legion/v1/state` or `kubectl get pod` instead of the daemon's internals
  - Asserting anything about how many workers are running
  - Matching pods to issues by label or by name
related_issues:
  - "LEGION-26"
  - "sjawhar/legion#1114"
---

# worker_cap is not a pod count

An outside check sees two things: the redacted state page and the cluster. Neither shows the
daemon's own running-worker count, and two of the kind smoke's checkpoints were first written
against facts that are not there.

## The cap bounds running-or-prompted workers; pods outlive that

`config.workerCap` bounds workers that are running or currently being prompted. A worker that
finished its task is idle, does not count, and its pod stays alive for `worker_idle_retire_seconds`
(default 600) before the idle-retire clock stops it. The redacted state page carries no run state.
So with `worker_cap: 1` the first `worker-cap` run saw the daemon queue `SLEGION26B-2/implementer`
while three worker pods were alive — two finished planners and one runner — and a check that
asserted `pods ≤ cap` at an instant would have called that a violation.

What is observable, and what `worker-cap` asserts instead:

- the daemon queued a task (`workerAdmission.queue` non-empty) while at least one worker pod was
  running — it judged its cap reached;
- the head of the queue was promoted once a runner finished (it left the queue and got a
  Pending/Running pod);
- an excess of worker pods over the cap never lasted longer than idle lingering can explain
  (`worker_idle_retire_seconds` + the stop timeout, `excess_allowance`, judged on wall-clock
  `date +%s`); a sustained excess is the violation, a transient one is reported in the OK detail.

The recipe sets `SMOKE_WORKER_IDLE_RETIRE=60` (the daemon's `worker_idle_retire_seconds`) so idle
pods stop hiding the count for ten minutes; a pod count is only ever an upper bound on the
daemon's. One more transient to tolerate: right after a promotion, zero worker pods can run for a
tick while the daemon awaits the old pod's deletion before creating the promoted one — a
zero-running sample counts only on consecutive samples with the queue held, and the counter
resets on an empty-queue tick.

## What the page and the pods expose

- `GET /legion/v1/state` has no `phases`, no `ompSessionFile` on a worker claim, no locators
  beyond `trees[].locator` and worker `roles[].locator`, and no `*Hash`/`*Secret`/`*Token`. The
  session id of a root comes from `roles[<architect role token>].sessionId`; the session file
  from `trees[<KEY>].locator.ompSessionFile`; the generation from `trees[<KEY>].generation`.
  `readyConfirmedAt` on the tree is the only "the replacement registered" signal.
- Pod labels `legion.dev/tree` and `legion.dev/issue` carry the **raw issue key**
  (`SLEGION26-1`); only pod, PVC, and Secret names are slugged (`legion-slegion26-1-architect-g1`).
  The plan assumed slugged labels; the checkpoint's `k8s_slug` helper was deleted once a live pod
  was read. Compare labels raw, names slugged.
- The root architect's pod is the one whose `legion.dev/issue` equals its `legion.dev/tree` with
  role `architect`; a sub-architect on a child has a different issue. `worker-cap`'s running set
  excludes exactly that pod and pods with a `deletionTimestamp`.

Rule: before writing a checkpoint, dump the state page and `kubectl get pod -o json` from a live
instance and design against what is there. A fact taken from the daemon's internals — a count,
a phase, a slug — is not observable from outside until the page says it.

## Related

- `packages/daemon/src/daemon/AGENTS.md`, `GET /legion/v1/state` and the `config.workerCap`
  bullet: the authoritative description of both.
- `docs/solutions/testing/live-proof-over-real-daemon-state-snapshots.md`: reading real state
  instead of hand-shaped fixtures.

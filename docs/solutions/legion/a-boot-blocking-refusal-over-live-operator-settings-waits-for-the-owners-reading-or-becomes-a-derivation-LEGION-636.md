---
title: "A boot-blocking refusal over live operator settings waits for the owner's reading before the plan is approved, or becomes a derivation"
category: legion
tags:
  - planning
  - configuration
  - legion-yaml
  - boot
  - image-probe
  - capacity-stop
date: 2026-10-16
status: active
module: packages/daemon/internal/config, packages/daemon/internal/daemon
related_issues:
  - "LEGION-636"
  - "LEGION-632"
  - "sjawhar/legion#1984"
---

# A boot-blocking refusal over live operator settings waits for the owner's reading before the plan is approved, or becomes a derivation

- A configuration-load refusal is boot-blocking: a daemon whose `legion.yaml` violates the new
  rule does not start at its next pin. Before a plan designs one over values an operator sets
  (`worker_boot_timeout_seconds`, `slow_command_timeout_seconds`, anything under
  `runtime.kubernetes`), the plan needs the live values of every deployment the release reaches.
- Nothing in-tree can read them. `deploy/kubernetes/daemon/legion.yaml.example` sets neither key;
  a worker pod's `kubectl get configmap` in `legion-system` is forbidden; the daemon logs print no
  configuration; `/legion/v1/state` carries no timeout. A coordinator's dated sentence that "both
  deployments hold the relation" is a hypothesis to check, not evidence; the one reader is the
  role that owns the deployments (Envoy role `legion-deploy`), asked through the architect — and
  asked before the plan is approved, not left to the implementer "if the pod's identity allows".
- Prefer the design that needs no reading. When the invariant is a relation between two keys the
  operator sets independently, derive the dependent value instead of refusing the pair:
  `daemon.ProbeBudget` makes the image probe's attempt `max(slow_command_timeout_seconds,
  worker_boot_timeout_seconds + 60 s)`, so the capacity stop can always see a probe pod's
  Unschedulable age pass the boot timeout inside one attempt. A derivation is never a refusal, and
  it stays visible: boot logs the derived bound with both keys, and `legion start --check-config`
  prints it after its OK line.
- Ask of any proposed refusal: what does it do to a deployment that already violates it? If the
  answer is "it stops booting", the acceptable designs are a warning or a derived bound, and the
  refusal needs the owner's reading of every live file first.

## Evidence (LEGION-636, sjawhar/legion#1984)

The spec's first item (from LEGION-632's tester) was that the probe's capacity stop could never
fire when `worker_boot_timeout_seconds ≥ slow_command_timeout_seconds`, and the coordinator's
merge-gate decision recorded both live deployments at 120 over the default 300. The plan's Task A
was a load refusal of that relation; its gap analysis named the claim unsupported in-tree and
deferred the read to the implementer. Before Task A was built, the architect asked the deploy
holder, who read both ConfigMaps: one daemon at 120 over 300, the other at 600 over 300 — raised
days earlier because a cold issue pod's two init containers took 311–344 s plus up to 84 s for a
node. The refusal would have stopped that daemon's next boot. The architect's ruling (spec v7)
replaced Task A with the derivation above, a boot log line, the `--check-config` line and a test
driving the stop under the 600-over-300 shape
(`TestProbeImagesCapacityStopNeedsTheBudgetToOutliveTheBootTimeout`, with the old shape as its
negative control). No `legion.yaml` of either deployment changed.

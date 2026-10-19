---
title: "A stage 4a negative on a second runtime clones the pod slices, proves its object is missing, captures the pod while it lives, and bounds the failure it excludes"
category: testing
tags:
  - stage-4a
  - live-harness
  - image-probe
  - negative-control
  - timing-bound
  - sandbox-runtime
date: 2026-10-19
status: active
module: packages/daemon/internal/runtime/sandbox/live_install_test.go
applies_when:
  - Adding a liveChecks row that drives ProbeImage, or any runtime call, with the operator fixture's pod altered
  - Asserting a wall-clock bound in a live check from an API-server timestamp
  - Answering a review thread that proposes changing a constant only the devbox harness evaluates
related_issues:
  - "LEGION-798"
  - "sjawhar/legion#2043"
---

# A stage 4a negative on a second runtime clones the pod slices, proves its object is missing, captures the pod while it lives, and bounds the failure it excludes

Extends docs/solutions/testing/negative-controls-on-shared-resources-use-fakes-and-markers-follow-the-resource.md.
That document keeps a destructive negative off a resource someone else owns; this is the Go form
for the stage 4a rig, where the shared resource is the run's own `Runtime` and its pod fixture.

- Build the negative on a second `Runtime` from `r.runtimeOptions(r.streamAddress(), r.ln)` and
  append only to `slices.Clone(r.pod.Volumes)` / `slices.Clone(r.pod.VolumeMounts)` (the sibling
  check does `maps.Clone(r.pod.Env)`). A bare `append(r.pod.Volumes, v)` writes into the shared
  backing array whenever capacity exceeds length and poisons every later check of the run, at one
  fixture size only.
- Prove the precondition the negative rests on, and tell "absent" from "could not ask": `kubectl
  get configmap <name>` must fail with `(NotFound)`; an object that exists fails the check naming
  it, any other error is returned as is. Without that read, "refused" and "refused for my reason"
  are the same line.
- Capture the thing you will measure while its producer still exists. `ProbeImage` creates,
  reads and deletes the probe Sandbox inside one call, so a `getPod` after it returns finds
  nothing; a goroutine polling `rt.storedPod(name)` keeps the last pod seen and hands it over a
  channel when the call returns, and the check fails explicitly ("unproven") when it saw none.
- Derive a timing bound from the constants it is made of and bound the failure you are
  excluding, not the claim you are making: `2*mountRecheck + 15*time.Second` excludes the
  71-minute budget wait as well as 30 s does, and tolerates one missed recheck, the poll's tick, the
  kubelet's first volume sync and the second truncation of the API server's `LastTransitionTime`
  against the runner's `time.Now()`. The claim itself ("within one read of the kubelet's first
  event") is shown by the noted durations, never asserted across two clocks.
- A review thread that changes a constant only the devbox harness evaluates is settled in the same
  round whatever its severity: no unit test can go red on it, and deferring it costs a full stage
  4a cycle and a re-review of the result.

## Evidence

`checkImageProbeMissingVolumeRefusal` (`live_install_test.go`, row
`image-probe-missing-volume-negative` in `live_test.go`, PR #2043) mounts a volume
`operator-missing` from ConfigMap `<operator ConfigMap>-missing` at `/etc/legion-operator-missing`
on cloned slices, guards the name with `kubectl get configmap … -o name` and a `(NotFound)` test,
runs the second runtime's `ProbeImage`, asserts `cannot mount volume "operator-missing"`, `ConfigMap
"<name>"`, either `configmap "<name>" not found` or `configmaps "<name>" not found`, and the
absence of `never completed within its retry budget`, then reads the last pod's
`PodScheduled=True` transition through `podCondition`. The bound began at 30 s; the reviewer's
not-blocking thread (round 1) showed it tolerated one missed 15 s recheck and compared two
clocks, and it was widened in the same round rather than after the first devbox run. The mount
entry exists because the kubelet skips a pod volume no container mounts
(`desired_state_of_world_populator.go` at v1.35.8), found while planning and not by running.

No pod of the issue's tree can run the check (`kubectl auth can-i create sandboxes.agents.x-k8s.io
-n legion` answers `no`); the production check asks the `legion-deploy` holder for its `CHECK
image-probe-missing-volume-negative: PASS` line and the noted `refused … after the pod was
scheduled` duration.

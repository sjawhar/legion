---
title: "A pre-stop wait is paid out of the pod's grace period, and the constant that sizes it lives in another module — raise both, and guard the relation from the side that owns the constant"
category: infra
tags:
  - kubernetes
  - prestop
  - terminationGracePeriodSeconds
  - rolling-update
  - endpointslice
  - pulumi
  - go-modules
  - golden-files
date: 2026-10-21
status: active
module: packages/envoy/infra/gcp/dispatch_kube.go
applies_when:
  - A Deployment that serves HTTP starts rolling instead of being recreated
  - A pod gains a preStop hook, or its terminationGracePeriodSeconds is chosen
  - A number one Go module renders is derived from a constant another Go module owns
related_issues:
  - "INFRA-62"
  - "sjawhar/legion#2089"
---

# A pre-stop wait is paid out of the pod's grace period, and the constant that sizes it lives in another module

- **A rolling update needs a pre-stop wait before it is honest.** A pod's removal from its
  EndpointSlice starts when the deletion timestamp appears, **concurrently** with the kubelet
  beginning termination, and must then reach every node's proxy and any ingress that keeps
  connections pinned to a backend. A server that closes its listener on SIGTERM is therefore sent
  requests it can no longer answer for the length of that propagation. The pre-stop hook is what
  runs *alongside* propagation and outlasts it. Design it at plan time, not after a reviewer asks
  what "no gap" means.
- **The wait is spent inside `terminationGracePeriodSeconds`, not before it.** The kubelet runs the
  hook first and sends SIGTERM only when it returns, both inside the one budget. A five-second wait
  in a thirty-second grace leaves the server twenty-five. So when you add a wait of `W` to a pod
  whose server is built for `S`, the pod asks for `W + S` — and say in the comment where each half
  comes from, because the next person tunes one of them.
- **Guard the relation from the module that owns the constant.** When the sizing constant is
  unexported in another module (`package main`, no exported surface), the dependent module can only
  copy the number, and a copy checked by nothing goes stale silently the first time the owner raises
  it. Put the test in the owning module instead: it reads its own constant directly and reads the
  dependent's rendered artefact — a golden, a manifest — and asserts the inequality. It then fires
  in the lane the person who edits the constant already runs. Delete the copy once the guard exists;
  two claims about one relation leave the decorative one for the next reader to meet first.
- **Make that guard fail, never skip.** A missing file, unexpected JSON, an absent field or a hook
  that is not the shape it reads are failures: a skip is the hole the guard was written to close.
  Compare durations rather than truncating to whole seconds, so a sub-second edit to the constant
  cannot pass.
- **A number that is derived belongs in prose as its arithmetic, not as a value.** "A 35-second
  grace period — a five-second pre-stop wait and the thirty seconds the server's shutdown is built
  for" cannot go stale invisibly the way a bare `35` can: whoever moves either half meets a sentence
  that is visibly about both.

## Evidence

`envoy-infra:gcp:Dispatch` (INFRA-62) began rolling its app Deployment at more than one replica.
`packages/envoy/cmd/dispatch/shutdown.go`'s `const stopGrace = 30 * time.Second` sizes the server's
whole shutdown sequence, whose last five seconds are the document service's reserve; a write
committing 27 s after SIGTERM is pinned by `cmd/dispatch/shutdown_test.go`.

- The first attempt added `lifecycle.preStop.exec.command: ["sleep","5"]` and left
  `terminationGracePeriodSeconds` at 30. The tester caught it: the wait came out of the server's
  shutdown, leaving 25 s where 30 is assumed, on every rolling update. The fix was 35, not a shorter
  wait.
- The infra module mirrored the number as `const dispatchServerStopGrace = 30` in its own test.
  Lowering the pod's grace failed that test; **raising `stopGrace` failed nothing**, because the
  mirror compared against its own stale copy. `packages/envoy/cmd/dispatch/pod_grace_period_test.go`
  replaced it: `package main`, reads `stopGrace`, parses
  `../../infra/gcp/testdata/dispatch-pod.golden`, asserts
  `time.Duration(grace-wait)*time.Second < stopGrace`. Proved by raising `stopGrace` to 45 and
  reading the refusal; the mirror and its test were then deleted in the same change.
- That comparison first read `int(stopGrace / time.Second)`, which truncated: at `30s500ms` the pod
  was half a second short and the guard passed. The duration comparison is what catches the one edit
  that otherwise slips through.
- Two comments described the propagation as happening *after* SIGTERM, which makes the hook
  pointless as written; they survived a review round because the conclusion they reached was the one
  everyone wanted. The README's own paragraph had it right, so the pull request shipped two texts
  disagreeing about the thing the round before had been spent making honest.

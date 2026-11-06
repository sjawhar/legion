---
title: "A projection fixed at pod creation needs a drift facet, and the list it joins is a data-safety decision"
category: daemon
tags:
  - sandbox
  - pod-drift
  - update-round
date: 2026-11-04
status: active
module: packages/daemon
related_issues:
  - "cybermonklabs/legion#2734"
---

# A projection fixed at pod creation needs a drift facet, and the list it joins is a data-safety decision

A pod projects some values once, when it is created, while the source of those values keeps
changing under it. The running pod can never catch up, and nothing says so unless a facet says it.

- When a pod's projection is fixed at creation and its source can gain entries later, add a facet
  that compares what the pod holds against what the daemon serves now. Do not add the rule to the
  launch's binding check, which answers a different question.
- **Register it in `driftedInPod` when the workers in the pod can be asked to push first.** The
  update round then replaces the pod under `ReplacePod`'s `due` guard. Register it in
  `movedInPod` only when no worker of the pod can be reached at all, because that path replaces the
  pod where it stands and takes the issue's disk with everything unpushed on it.
- Register it **once**. `movedInPod` already appends every `driftedInPod` facet when
  `!roundCovers(podKindFor(role))`, which is what gives a pod no round covers — the project
  controller's — its replacement at the next launch for free.
- **Run the comparison one way.** A remedy that destroys data is keyed to what the pod *lacks*;
  something the pod holds that the daemon no longer serves is a stale directory and no reason to
  remake a pod. This is the more reusable half of the rule: the cost of the remedy decides the
  direction of the check.
- Give the derivation one name and have both sides call it: the side that **builds** the pod's
  value and the side that **judges** the pod against it. Wire the judging side as a required
  construction option, refused at construction, so no runtime can compare against nothing. In the
  rig, derive the expected set from the fixture set rather than hand-listing it. Where a value is
  encoded into a key, write the decoder beside the encoder over one shared private constant.
- **Pin the cardinality of the drift message, not only its content.** Adding a facet to a list
  that drives an operator-visible message can multiply the reasons a single cause produces.
- Split the tests by cost: a facet taking the narrow argument it needs lets its refusal cases go in
  a hand-built spec table, and only the live paths pay for the rig.
- In the pull request body, say that the first daemon carrying a new facet drifts every pod that
  predates it **at once**, and name the guard that makes that safe and the rollout order it
  implies.

## Evidence

cybermonklabs/legion#2734 added `movedGhOwners`
(`packages/daemon/internal/runtime/sandbox/addresses.go:400`). A pod's gh directories come from one
Secret volume per role whose `Items` are fixed when the pod is created
(`manifest.go:684-711`, built from the launch's `gitHubOwners`), while the credential refresher
adds a newly served owner's two keys to the live Secret afterwards (`ghcredential.go`). So a pod
made before an owner was served has no directory of that owner's name, and every `gh` call of its
roles against that owner's repositories runs unauthenticated.

- It is registered in `driftedInPod` only. The launch path would set `replace = true` at
  `relaunch.go:169-178` and end the pod where it stands; `drift.go:17-22` states the cost — "ending
  it ends each of their processes mid-turn and deletes the disk they work on, with everything
  unpushed on it". The guard is `ReplacePod`'s `due` argument, asked at `drift.go:340`.
- `movedInPod`'s `if !roundCovers(podKindFor(role))` (`addresses.go:346-348`) gives the project
  controller's pod the same facet with no second registration;
  `TestAControllerPodWhoseGhVolumeLacksAServedOwnerIsReplacedByItsNextLaunch` pins that.
- The one-way rule is stated at `addresses.go:396-399` and driven by the tester: a pod holding
  `{acme, sjawhar}` under a daemon serving `{acme}` alone reads no drift.
- One derivation, two readers: `appauth.projectOwners` with `appauth.GitHubOwners` over it
  (`packages/daemon/internal/appauth/credential.go`), read by `GitHubCredential` and wired into the
  runtime at `internal/daemon/kubernetes.go:487`; `sandbox.go`'s `checkSupervision` refuses
  `opts.GitHubOwners == nil` beside the existing `GitHubCredential` refusal, and
  `manifest_test.go`'s refusal table covers it. The encoder/decoder pair is `names.go`'s
  `githubHostsPrefix` with `GitHubHostsKey` and `GitHubHostsOwner`. The rig derives
  `servedOwners` from `staticOwners` (`rig_test.go:153-156`).
- Cardinality: `restage_test.go` asserts exactly one reason names the gh volume — "the gh directory
  is one volume, whatever it lacks".
- `movedGhOwners` takes a `corev1.PodSpec`, so its refusal cases are table-tested against
  hand-built specs while the rig, about 270 s for the package, carries only the live paths.
- The rollout risk is recorded in the pull request body: the first daemon carrying this facet
  drifts every issue pod made before the new owner was mapped, which is safe only with the
  unpushed-work guard, and the architect owns that order.

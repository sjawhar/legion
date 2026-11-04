---
title: "Absent and present-but-empty are two states, and one nil check cannot answer both"
category: best-practices
tags:
  - state-modelling
  - go
  - kubernetes
date: 2026-11-04
status: active
module: packages/daemon
related_issues:
  - "cybermonklabs/legion#2734"
---

# Absent and present-but-empty are two states, and one nil check cannot answer both

- Before writing `if x == nil { return "nothing to do" }`, name every state that `nil` covers and
  decide each on its own. A container the system never made is **unknown**: leave it alone. A
  container the system made deliberately to hold nothing is **known empty**: act on it. The same
  `nil` answers for both and the fold is invisible.
- Split them on different facts: the thing's **presence** in the list it would be in, and then the
  thing's **shape**.
- Where the empty case has a distinct encoding, cite in the comment the construction site that
  produces it, so a reader can check the claim rather than take it.
- A fold like this survives every test written from the common case. The test that catches it is
  the one that starts the system in the deliberate-empty state.

Related: docs/solutions/envoy/a-callee-that-folds-two-sentinels-makes-its-callers-arm-order-load-bearing-INFRA-249.md
— the same family, a fold hiding a distinction, by a different mechanism.

## Evidence

cybermonklabs/legion#2734, `movedGhOwners` in
`packages/daemon/internal/runtime/sandbox/addresses.go`. Its first cut read

```go
if at < 0 || pod.Volumes[at].Secret == nil {
    return movedAddress{}, false
}
```

which folded a pod with **no** gh volume, which is no pod of this runtime's making, together with
a gh volume that **is** an `EmptyDir`. The second is not unknown: `manifest.go:703-711` builds an
`EmptyDir` exactly when `len(ghItems) == 0`, because a Secret projection with no items would
project every key of the Secret, the launcher token among them. So it is the empty owner set, and
the first owner its project gains must remake that pod.

The shipped form splits them — absence is `at < 0` over `pod.Volumes`, emptiness leaves `held`
empty and falls into the ordinary comparison — and the doc comment cites the construction site.

The state that exposed it is ordinary and transient: a project whose mapping names no repository
both Apps are installed on yet, or whose installation read GitHub did not answer, leaves the
controller with an empty gh directory. Every test of the common case passed over the fold; the one
that failed is the tester's `TestAnOwnerlessControllerPodIsReplacedOnceItsProjectGainsAnOwner`
(`drift_launch_controller_test.go`), which starts the controller with no owner at all, and it is
the single test that fails again when the old guard is restored by hand.

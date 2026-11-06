---
title: "A Pulumi component's declaration is pinned by a sorted golden captured from the base"
category: testing
tags:
  - pulumi
  - golden
  - mock-engine
  - envoy-infra
date: 2026-10-30
status: active
module: packages/envoy/infra
related_issues:
  - "INFRA-774"
  - "sjawhar/legion#2528"
---

# A Pulumi component's declaration is pinned by a sorted golden captured from the base

"The existing path declares what it declared before" is the hardest claim a component change makes,
and a type-count assertion does not hold it. Pin it like this.

- **Capture the golden from the base commit, in a commit of its own, before you touch the
  component.** A golden written after the change pins the change. Say in the test's doc comment
  that a failure means the code moved and the fix is the code, never the golden.
- **Sort the lines before comparing.** The Pulumi Go SDK registers each resource from a goroutine
  of its own, so the order the mock engine records is the scheduler's, not the component's. One
  line per registration — `<type> <name> parent=<urn>` — still pins every resource, its name and
  its parent.
- **Derive a second configuration's expectation by filtering that golden, never by writing the
  lines out again**, and assert the filter matched as many lines as types you asked for. A
  hand-written subset stays green while the two paths drift: a resource the default path renames
  or reparents moves the golden and leaves the copy behind.
- **A golden over type, name and parent does not cover a changed property, a changed dependency or
  a reordering.** When you move a registration or split the function that makes it, prove it with
  a throwaway dump of everything — parent, provider, dependencies, property dependencies, resource
  options and the whole resolved input map — taken at the base and at your head and compared byte
  for byte. Delete the dump; it is evidence, not a test.
- **Add no assertion a whole-set comparison already entails.** Per-type counts, a prefix loop and
  a "this provider is absent" check next to a comparison of the entire registration set cannot
  fail on their own, and each one is a second copy of the type list that must be edited with it.
- **Justify deleting such an assertion by mutating the component, not by arguing.** Break the
  thing the deleted assertion watched and show the surviving comparison failing and naming it.
- **Hold any per-input table complete by reflecting over the args struct** and comparing the full
  set of `pulumi:` tag names against the names the table covers, so an input added later cannot
  escape it in silence.

## Evidence

sjawhar/legion#2528. `packages/envoy/infra/gcp/testdata/dispatch-registrations.golden` was
committed in `76eb954d`, before any change to the component, and the feature commits left it
byte-identical. `dispatchRegistrations` renders and sorts the lines
(`gcp/dispatch_test.go:1554`, `slices.Sort` at `:1564`), and
`TestDispatchRegistrationsMatchTheGolden` (`:1573`) compares them. The sort is not a preference:
three runs of the same program on the branch's base recorded three different orders, and a golden
captured from one run failed ten of ten later runs.

`dispatchClusterOnlyGolden` (`gcp/dispatch_cluster_only_test.go:48`) filters that same golden down
to the component, the test's provider and the five cluster resources, with
`require.Len(t, lines, len(keep))` (`:57`) so a filter that stops matching fails rather than
shrinking, and the cluster-only test compares the whole registration set against it (`:77`).
`TestDispatchEveryInputIsLiveOrRefusedUnderClusterOnly` (`:191`) holds the 46-row refusal table
equal to `DispatchArgs`' tag set, in the shape of `packages/envoy/infra/aws/seeds_test.go:478`.

The change reordered the default path's private-services pair after the cluster. The golden could
not see that; what licensed it was two throwaway dumps of every registration with its parent,
provider, dependencies, options and inputs, taken at the base and at the head — 498 lines each in
the tester's run, 4,561 lines across seven argument sets in the reviewer's — each pair identical.

The last two rules cost a review round. Round 1 carried a twelve-entry count map, a `kubernetes:`
prefix loop, a `pulumi:providers:kubernetes` check and a second test whose only claim was the first
test's own `require.NoError`; all four were entailed by the comparison three lines above them
(review comments `#discussion_r4419036573`, `#discussion_r4419036580`, fixed in `5d65f97b800f`).
What settled it was mutation: with a `compute.GlobalAddress` re-added to the cluster half, the
single surviving comparison failed at `gcp/dispatch_cluster_only_test.go:77` naming
`gcp:compute/globalAddress:GlobalAddress`, and the round-2 deep review's three further mutations —
a Kubernetes provider, an unconditional `random` requirement, the early return disabled — were each
caught by the same test.

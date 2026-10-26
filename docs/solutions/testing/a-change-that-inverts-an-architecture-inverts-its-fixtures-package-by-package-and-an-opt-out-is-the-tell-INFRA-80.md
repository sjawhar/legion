---
title: "A change that inverts an architecture inverts its fixtures, package by package, and a test that opts out of the new world is the tell"
category: testing
tags:
  - fixtures
  - test-doubles
  - architecture-change
  - census
date: 2026-10-25
status: active
module: packages/daemon/internal/runtime/sandbox, packages/daemon/internal/daemon
related_issues:
  - "INFRA-80"
  - "sjawhar/legion#2173"
---

# A change that inverts an architecture inverts its fixtures, package by package, and a test that opts out of the new world is the tell

A string sweep cannot find this class, which is why it needs its own rule: a fixture that builds
the old architecture and one that builds the new use the *same* identifiers. Grepping
`volumeClaimTemplates` returns both worlds and tells you nothing.

- **Make the fixture's default the architecture the change establishes, and give the old shape a
  named opt-in with at least one caller.** An opt-in with no caller is dead fixture code; if a
  migration test ever needs the old shape, that test and its helper arrive together.
- **A test that opts *out* of the current architecture is the tell that a package was missed.**
  Look for the shape `fixture(...)` immediately followed by a line deleting what the fixture just
  added — the test is working around a default that should be its default.
- **Inverting is per package, and the sweep is `grep -rn '<fixture constructor>('` across every
  package that has one.** One package's inversion says nothing about its neighbour's: here the two
  were inverted five rounds apart, and the neighbour's own test carried the opt-out in the
  meantime.
- **Prove the opt-in is load-bearing before trusting it.** Name the test that fails without it. An
  inversion's one failure mode is a migration test that silently stops exercising the old shape and
  keeps passing.
- **Inert today is not the argument.** A fixture nothing launches still teaches the next reader
  what the normal object looks like, and the first test that *does* launch into it takes the
  migration path, passes, and proves the wrong thing.

## Evidence (INFRA-80, sjawhar/legion#2173)

The change made an issue pod's disk the pod's own generic ephemeral volume, where the issue's
Sandbox had owned a claim template that outlived every pod.

`internal/runtime/sandbox`'s `sandboxObject` was inverted in round 6: it builds no claim template,
and `withSupersededDisk` (`rig_test.go`) is the named opt-in, used by the three cutover tests.
`TestALaunchReplacesTheIssueSandboxOfTheSupersededDiskShape` requires a delete of the superseded
Sandbox followed by a create — which a Sandbox owning no template never produces — so the opt-in is
load-bearing and its removal fails the test rather than passing quietly.

`internal/daemon`'s `issueSandbox` was not inverted until round 11, five rounds later. The tell was
already in the tree: `carried_child_cleanup_test.go`'s `launchedChildSandbox` called `issueSandbox`
and then ran `delete(s["spec"].(map[string]any), "volumeClaimTemplates")` to get the shape the
release actually makes — an opt-out of the current architecture, inside the test whose own point is
that an issue's disk goes with its pod.

No opt-in was added to that package: none of its seven call sites means the superseded shape, and
the fake API never reads `spec.volumeClaimTemplates` at all — its `pvcs` flag is set from a
Sandbox's existence and cleared by a foreground delete. That reading is what made the inversion
safe, and it is stronger than "none of them launches", which a later test that launches would
overturn.

A third fixture, `boundary_test.go`'s volume mapping, was *added by the change itself* and modelled
the world the change abolishes: it mapped a generic ephemeral volume to the rig's long-lived shared
directory, so a pod's own disk aliased one that outlives it, and the plant vectors provisioned a
second pod against the first pod's files. Restoring that alias after the fix fails 9 of 9 vectors —
the rig could not tell the two worlds apart while it stood.

The vocabulary moves with the default: `internal/daemon`'s `pvcs`/`pvc()`, documented as the claim
"the Sandbox owns", became `disks`/`disk()` for the claim the *pod* owns, as the sibling package's
`cleanup_test.go` had already done. A fixture that keeps the old ownership in its names teaches it
one layer below the shape it just gave up, and that layer is not visible in a diff of defaults.

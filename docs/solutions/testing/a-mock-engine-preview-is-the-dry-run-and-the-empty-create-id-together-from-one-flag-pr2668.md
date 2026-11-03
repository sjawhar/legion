---
title: "A mock-engine preview is the dry run and the empty create ID together, from one flag"
category: testing
tags:
  - pulumi
  - pulumi-mocks
  - preview
  - envoy-infra
date: 2026-11-02
status: active
module: packages/envoy/infra/internal/pulumitest
related_issues:
  - "sjawhar/legion#2668"
---

# A mock-engine preview is the dry run and the empty create ID together, from one flag

Extends docs/solutions/testing/a-helm-v4-chart-under-pulumi-mocks-registers-no-children-pin-its-values-and-render-the-vendored-chart-pr2371.md.

- A Pulumi mock preview has two halves: the program runs with `RunInfo.DryRun` set, and the mocks
  answer a create with no ID, so the resource's ID and every output built on it are unknown. A
  test that sets only the first proves nothing about an unknown, because every create still
  answers with an ID.
- Drive both halves from one flag on the mocks, and have the harness that constructs through the
  plugin read its dry run from that same flag, so the engine's preview and the mocks' answer
  cannot disagree. A separate `dryRun` parameter beside a mocks field is the shape that lets them.
- A create's empty ID is the real provider's behaviour, not a convenience of the harness: a preview
  create returns an empty ID.
- Assert `IsNull() || IsComputed()` on the preview row. The mock monitor unmarshals a
  registration's inputs without keeping unknowns, so an unknown arrives as null, and an assertion
  written for `IsComputed()` alone fails on a correct implementation.
- Pin the apply value in the same test. The preview row says the value waits; only the apply row
  says it waits for the right thing and resolves to what the live object already holds.
- Read a component's preview property on both surfaces: the component under the mocks, and the
  component as the plugin serves it through `Construct`, which takes its dry run from the request.
  A pass on the component alone leaves the artifact a consumer installs unread.

## Evidence

sjawhar/legion#2668 made the front door's RoleBinding take `roleRef.name` from the Role's ID, so
that a preview creating the Role leaves the name unknown. Proving that needed a preview the
harness could not express: before this change `packages/envoy/infra/internal/pulumitest/mocks.go`
had no preview at all, and `constructDispatch…`'s `dryRun` parameter set the request's flag while
every create still answered with `<name>-id`. The existing preview rows of
`gcp/dispatch_construct_test.go` therefore could not have caught this class of defect.

What the change added: `mocks.go:42` (`Preview bool`), `mocks.go:70`
(`if m.Preview && args.ReadRPC == nil { id = "" }`, a read keeping its ID since what it reads
exists) and `mocks.go:138` (`info.DryRun = true`). `gcp/dispatch_construct_test.go:86`,
`constructDispatchOnMocks`, takes the mocks the caller owns and passes `m.Preview` as the
`Construct` dry run, so the two cannot disagree.
`gcp/dispatch_test.go`'s `TestDispatchFrontDoorBindingWaitsForItsRoleOnPreview` runs the four rows
this rule asks for: component and plugin, each at `preview=false` and `preview=true`, asserting
the unknown under preview and the exact `example-dispatch-tailnet-tls` on the apply.

Fidelity: pulumi-kubernetes v4.34.2 (`packages/envoy/infra/go.mod:22`)
`provider/pkg/provider/provider.go:1889-1893` returns `id := ""` for every preview create, which is
what `Mocks.Preview` reproduces.

The negative control for such a test — running the unchanged test files over the tree the branch
started from — is this repository's standing practice and is written up already; see
docs/solutions/testing/prove-a-go-acceptance-test-fails-on-base-by-running-the-unchanged-file-in-a-git-archive-export-INFRA-487.md.

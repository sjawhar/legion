---
title: "A consumer's DependsOn on a remote component reaches no child, so a child that must follow one takes an ordering input"
category: infra
tags:
  - pulumi
  - component
  - dependson
  - envoy-infra
date: 2026-10-31
status: active
module: packages/envoy/infra
related_issues:
  - "sjawhar/legion#2584"
---

# A consumer's DependsOn on a remote component reaches no child, so a child that must follow one takes an ordering input

- **Do not expect `pulumi.DependsOn` on a remote component to order the component's children.** It
  becomes the component's own resource option, and each child's register request carries only the
  `DependsOn` the component gave that child. A consumer that must run one of its own resources
  before one child of your component cannot say so with the component-level option.
- **Give that child an explicit ordering input**, documented as a value from the resource the
  child must follow, whose content the component never reads. Render it with the
  `DependsOnInputs` idiom, which takes the dependencies the output carries and discards the value:

  ```go
  func dependsOnInput(in pulumi.StringInput) pulumi.ResourceOption {
      return pulumi.DependsOnInputs(in.ToStringOutput().ApplyT(func(string) []pulumi.Resource { return nil }).(pulumi.ResourceArrayOutput))
  }
  ```

- **Chain the input through the validator it already owes rather than beside it.** `ApplyT` keeps
  the source output's dependencies, so `dependsOnInput(refuseSeed(args.X, …))` both refuses a bad
  value and orders the child. A second, unvalidated read of the same input is how a check gets
  skipped.
- **Measure the claim in the harness, never read it off the SDK.** Construct the component through
  the plugin's own `Construct` with a URN in `ConstructRequest.Dependencies`, the field that option
  arrives in, and read the child's recorded register request. Write the test to what you observe.
- **An absence assertion needs a liveness guard and must not pin the whole list.** Require that the
  dependency list is non-empty, so the absence is a read and not an empty answer, then require it
  does not contain the URN. A `require.Equal` over the whole list fails on any unrelated sibling
  dependency added later.

## Evidence

sjawhar/legion#2584, `envoy-infra`'s `gcp:Dispatch`.

- The measurement is `TestConstructComponentDependsOnDoesNotReachTheChildren`
  (`packages/envoy/infra/gcp/dispatch_construct_test.go`): constructed with one upstream URN in
  `ConstructRequest.Dependencies`, the Cloud SQL instance's recorded dependencies held its own two
  siblings, the network and the private-services connection, and never the consumer's URN.
  `internal/pulumitest/mocks.go` gained a trailing variadic `dependsOn ...resource.URN` solely to
  make that measurable; variadic and last, so no existing caller changed.
- The supported path is the `databaseWaitsFor` input: `dependsOnInput` at
  `packages/envoy/infra/gcp/dispatch_kube.go:241`, wired into the instance's options at
  `dispatch_cloud.go:178` as `dependsOnInput(refuseDispatchSeed(args.DatabaseWaitsFor, dispatch,
  "databaseWaitsFor"))`, so the seed refusal and the ordering come from one read.
- The planner's reading of `pulumi/sdk/v3`'s `provider.go` predicted this, and the plan still
  required it measured; the explicit input was built either way, which is why the answer cost no
  round.
- The liveness guard is `dispatch_construct_test.go:162`. The first version of that test pinned the
  instance's whole dependency list with `require.Equal`; the reviewer required `NotEmpty` plus
  `NotContains` instead, because the finding is the consumer's URN not arriving and nothing else.

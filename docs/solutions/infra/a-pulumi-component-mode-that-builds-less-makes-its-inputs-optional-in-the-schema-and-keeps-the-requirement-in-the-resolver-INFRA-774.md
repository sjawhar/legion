---
title: "A Pulumi component mode that builds less makes its inputs optional in the schema and keeps the requirement in the resolver"
category: infra
tags:
  - pulumi
  - component-provider
  - schema
  - envoy-infra
date: 2026-10-30
status: active
module: packages/envoy/infra
related_issues:
  - "INFRA-774"
  - "sjawhar/legion#2528"
---

# A Pulumi component mode that builds less makes its inputs optional in the schema and keeps the requirement in the resolver

An input that makes a component register a subset of what it registers today — a cluster without
the application on it, a queue without its consumers — is five changes, not one. Make all five.

- **Make every input of the part you no longer build optional in the served schema**, by adding
  `,optional` to its `pulumi:` struct tag. Schema optionality comes from that suffix alone, never
  from the Go field's type: a `*bool` with the bare tag is a *required* schema input. A generated
  SDK's constructor refuses a nil required input before the plugin is ever called, so a consumer
  in the new mode cannot call the component at all while they stay required. Leave every Go field
  type where it is.
- **Keep the requirement in the resolver**, in the wording it already used, so the old mode refuses
  exactly as before. Nothing on the provider side enforces required-ness: `infer`'s Construct
  validates enums alone.
- **Refuse by name, in the new mode, every input that configures the part you no longer build.**
  Ignoring it is the dangerous choice: an applied stack that keeps its inputs could otherwise flip
  the flag and have the next update delete live infrastructure. Judge "set" as the module already
  judges it — a plain string when it is not empty, a slice when it has an element, a pointer or a
  `pulumi.*Input` when it is not nil — so a value unknown at preview is set as it will be at
  update. Say in the operator document what this does *not* stop: a consumer that strips every one
  of those inputs and turns the mode on in one change is not refused, and only the resource's own
  deletion protection stands in the way.
- **Require exactly the providers the path you take registers with.** Gate each
  `providers.Require` on the mode; an unconditional one compiles, passes every default-path test,
  and makes a consumer declare a provider it never uses.
- **Assign every output an explicit value on the short path.** An output field left unset awaits as
  *unknown*, which is one value in preview and another in update; an explicit
  `pulumi.String("").ToStringOutput()` is the same in both, is what a test can assert, and is
  usually what a downstream stack already reads as "not applied". Never make an output optional to
  express this: that changes its type for every consumer, the unchanged mode's included.

Hold the refusal list complete with a reflection test over the args struct rather than by hand, and
assert the outputs under both `dryRun` true and false. `docs/solutions/testing/a-pulumi-components-declaration-is-pinned-by-a-sorted-golden-captured-from-the-base-INFRA-774.md`
has those test shapes.

## Evidence

sjawhar/legion#2528 added `clusterOnly` to `envoy-infra:gcp:Dispatch`
(`packages/envoy/infra/gcp/dispatch.go:207`). Thirteen inputs took `,optional`, so the served
schema's `requiredInputs` fell to `region` and `hostname`
(`packages/envoy/infra/provider/provider_test.go:261`); changing the new field's tag to the bare
`pulumi:"clusterOnly"` failed that test with `elements differ … "clusterOnly"`. A Go SDK generated
by `pulumi package gen-sdk` from the plugin built at the head compiled a program that constructs
the component with the mode, the region and the hostname and nothing else; the same program with a
plain `string` for `tailnetDnsName` failed with `cannot use … (untyped string constant) as *string
value in struct literal` and compiled once wrapped in `pulumi.StringRef`.

The refusal table is `dispatchClusterOnlyRefusals` (`gcp/dispatch.go:523`), one row per
`pulumi:`-tagged field but the four the mode keeps live, looped at `gcp/dispatch.go:616`;
`TestDispatchEveryInputIsLiveOrRefusedUnderClusterOnly`
(`gcp/dispatch_cluster_only_test.go:191`) holds it equal to the struct's tag set. The `random`
requirement is gated at `gcp/dispatch.go:1130`, and a program declaring a `gcp` provider alone
succeeds; the round-2 deep review made the requirement unconditional again and that `require.NoError`
is what failed. The five outputs the mode does not decide are set at `gcp/dispatch.go:1151`, and
`TestConstructOutputsTheClusterOnlyState` (`gcp/dispatch_construct_test.go:461`) asserts all eight
through the plugin's own Construct for an update and a preview, each `value.IsString()` — known,
never unknown (`:481`).

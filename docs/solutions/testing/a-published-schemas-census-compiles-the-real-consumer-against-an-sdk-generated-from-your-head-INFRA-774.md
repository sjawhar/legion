---
title: "A published schema's census compiles the real consumer against an SDK generated from your head"
category: testing
tags:
  - census
  - pulumi
  - schema
  - envoy-infra
date: 2026-10-30
status: active
module: packages/envoy/infra
related_issues:
  - "INFRA-774"
  - "sjawhar/legion#2528"
---

# A published schema's census compiles the real consumer against an SDK generated from your head

Extends docs/solutions/testing/a-required-option-this-branch-added-is-a-census-item-too-mains-new-constructor-call-sites-fail-at-runtime-not-at-compile-time-LEGION-631.md.

That note censuses names this branch made required, found by grepping the repository. A component
provider's schema is a contract with code that is not in this repository and does not compile
against your source at all: it compiles against an SDK a consumer generates from your released
plugin. Census it like this.

- **Grep this repository first, and expect the answer to be "no caller".** `packages/envoy/infra`
  is consumed through its generated SDK, so hits here are usually comments, changelog fragments
  and the component's own tests. Record them with their disposition anyway; a census that lists
  nothing is indistinguishable from a census nobody ran.
- **Then read the consumer repository's own call site and compile it.** Build the plugin at your
  head, run `pulumi package gen-sdk` against that binary, and compile the consumer's real argument
  literal against the SDK it produces. Do not reason about which fields break: the compiler names
  them, and it names the ones you would have missed.
- **Judge an input-type widening from the SDK's own type set, not from the declared type.** In
  Pulumi's Go SDK both `pulumi.String` and `pulumi.StringOutput` carry `ToStringPtrOutput`, so an
  input that widens from `pulumi.StringInput` to `pulumi.StringPtrInput` still takes either, and
  only a value whose *static* type is the `pulumi.StringInput` interface stops compiling. A plain
  `string` input that becomes `*string` always needs a `pulumi.StringRef(…)` at every call site.
- **Count the inputs the consumer does not set yet.** An input the consumer's pinned version does
  not have is not a wrap it must add; it is an input it must start passing, because your resolver
  requires it. That item is invisible to a diff of the two SDKs.
- **Say what reaches a running stack and when.** A consumer that pins a released version meets
  nothing until it moves that pin, and a refusal gated behind a new opt-in input reaches no
  existing caller at all. Put the pin move in its own issue and prove here only that the
  declaration is identical.
- **An SDK-visible type change ships as `feat(<package>)!:` in the pull request title**, which is
  what cuts the major release, and the changelog fragment names each type move and the wrap a
  consumer writes.

## Evidence

sjawhar/legion#2528 made thirteen inputs of `envoy-infra:gcp:Dispatch` optional in the served
schema. In sjawhar/legion the three searches found no caller outside
`packages/envoy/infra` itself: seven hits, every one a comment or a changelog fragment.

The consumer is theorem-labs/agent-infra, read read-only with
`gh api repos/theorem-labs/agent-infra/contents/<path> -H "Accept: application/vnd.github.raw"`.
Its `dispatch/main.go` holds one `DispatchArgs` literal (lines 746-775) and one `NewDispatch` call
(line 816). Compiled against an SDK generated from the plugin built at the head, that literal
failed with exactly six errors and compiled clean once those six were wrapped in
`pulumi.StringRef` — `tailnetDnsName`, `envoyImage`, `tailscaleImage`, `cloudSqlProxyImage`,
`signInClientId` and `signInDomain`, each fed from a plain `string` field of its settings struct.
Four more of its inputs needed nothing: a `pulumi.StringArray`, two `pulumi.String(…)` and one
`pulumi.StringOutput`, which satisfy `pulumi.StringPtrInput` by
`github.com/pulumi/pulumi/sdk/v3/go/pulumi/types_builtins.go:9449,9483`. The seventh plain input
that changed type, `frontDoorToolsImage`, is set nowhere in that program, because the pinned
2.9.1 has no such input; the pin move has to add it, since the resolver requires it with the new
mode off. `legion/main.go` reads the component's outputs through a `pulumi.NewStackReference`
(lines 770 and 953-967) and needed nothing, no output's type having changed.

Both of agent-infra's stacks pin `envoy-infra@2.9.1` in `Pulumi.yaml`, and its
`envoy/envoy-infra-version.sh` refuses a pin that disagrees with the generated SDK, so nothing
reached a running stack; the pin move is dispatch://INFRA-775. The release is a major because the
title reads `feat(envoy-infra)!:`, which `.github/scripts/release-bump.sh` reads.

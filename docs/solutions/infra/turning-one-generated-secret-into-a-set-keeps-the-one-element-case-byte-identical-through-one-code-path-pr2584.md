---
title: "Turning one generated secret into a set keeps the one-element case byte-identical through one code path"
category: infra
tags:
  - pulumi
  - secrets
  - rotation
  - envoy-infra
date: 2026-10-31
status: active
module: packages/envoy/infra
related_issues:
  - "sjawhar/legion#2584"
---

# Turning one generated secret into a set keeps the one-element case byte-identical through one code path

A component that generates one secret and later must hold several — a signing key set a rotation
walks through, a key list a server parses — changes what every running deployment's Secret holds.
The first apply of the new release must plan no change to it.

- **Keep the first element's resource name and its generator arguments byte for byte.** The key a
  running deployment already has is the one the component keeps; anything else replaces it, and
  the replacement is the outage the change exists to avoid.
- **Render the one-element case through the same code path as the many**, not through a special
  case beside it. `strings.Join` of a one-element slice returns that element unchanged, so one
  typed join over a `pulumi.StringArray` gives identical bytes and keeps the value's secretness,
  where a branch plus an untyped `[]any` path adds an error return that cannot happen and two
  renderings to keep in step.
- **Pin the whole input map of the first element's resource, not just the rendered value.** A test
  that reads the resolved bytes alone passes while a keeper, a changed length or a special-character
  setting replaces the resource underneath it.
- **Say what the pin does not prove.** Mocks prove the declaration; only a `pulumi preview` on the
  real stack proves what that stack would plan, and that belongs to the change that moves the pin.

## Evidence

sjawhar/legion#2584 gave `envoy-infra`'s `gcp:Dispatch` a `signingKeyGenerations []int` input for
`DISPATCH_SIGNING_KEY`.

- `packages/envoy/infra/gcp/dispatch_cloud.go:208`: generation 0 keeps the name
  `<hostname>-signing-key` and the same `RandomPasswordArgs` (`Length 64`, `Special false`, no
  keepers) the single key always had; generation *n* above 0 is `<hostname>-signing-key-<n>`.
- `packages/envoy/infra/gcp/dispatch_kube.go:354`: one `pulumi.StringArray` of the results joined
  in one `ApplyT` on its `ToStringArrayOutput`. The first version had a one-key branch returning
  the bare result and an `[]any`/`outputStrings` path beside it whose error branch was unreachable,
  because `RandomPassword.Result` is already a `StringOutput`; the reviewer required the single
  typed flow.
- `TestDispatchDefaultSigningKeySetIsTodaysOneKey` (`gcp/dispatch_test.go:1031`) asserts the
  resolved bytes **and** the resource's whole input map, so any added argument fails there.
- The pull request's `Not proven / risk` says plainly that no live preview ran: this pod had no
  `pulumi` CLI and no cloud credential, and the consumer did not pin the release yet.
